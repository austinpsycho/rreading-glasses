package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dgraph-io/ristretto/v2"
	"github.com/go-chi/chi/v5/middleware"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

// ABGetter implements a getter backed by Audible, via audnexus for normalized
// detail lookups and Audible's own catalog API for search.
//
// Unlike the Goodreads and Hardcover upstreams, Audible has no concept of a
// "work": every ASIN is one narration in one marketplace. Rather than
// synthesizing works by grouping titles — which is exactly the fuzzy matching
// this upstream exists to avoid — one ASIN is modelled as one work containing
// exactly one edition. Work and edition therefore describe the same product
// but carry distinct surrogate IDs, since the client requires them to differ.
type ABGetter struct {
	cache  cache[[]byte]
	client *AudibleClient
	ids    *IDMapper

	// authors, ratings and products are bounded. An import touches hundreds of
	// thousands of records, and holding every one for the life of the process
	// is what makes memory climb until the container is killed. These are
	// caches, not indexes -- a miss costs a request, not correctness.
	authors  *ristretto.Cache[string, *audnexusAuthor]
	ratings  *ristretto.Cache[string, *audibleRating]
	products *ristretto.Cache[string, *audibleProduct]
}

// Cache sizes are counted in entries. Products are the largest by far, holding
// a full catalog record each, so they get the smallest allowance: they only
// need to outlive the bulk load that follows a search or the walk that found
// them.
// authorFailureTTL is how long a failed author lookup is remembered. Long
// enough that a rate limit isn't retried per book, short enough that a
// transient failure doesn't leave an author blank.
const authorFailureTTL = 10 * time.Minute

// _ratingTTL is how long a rating is kept outside the in-memory cache.
// Ratings move slowly and a stale one is harmless; an absent one is not.
const _ratingTTL = 30 * 24 * time.Hour

const (
	authorCacheSize  = 20_000
	ratingCacheSize  = 100_000
	productCacheSize = 25_000
)

func newRecordCache[V any](entries int64) *ristretto.Cache[string, V] {
	c, err := ristretto.NewCache(&ristretto.Config[string, V]{
		NumCounters: entries * 10,
		MaxCost:     entries,
		BufferItems: 64,
	})
	if err != nil {
		panic(err)
	}
	return c
}

var (
	_ getter     = (*ABGetter)(nil)
	_ asinLookup = (*ABGetter)(nil)
)

// NewAudibleGetter returns a new getter backed by Audible.
func NewAudibleGetter(cache cache[[]byte], client *AudibleClient, ids *IDMapper) (*ABGetter, error) {
	return &ABGetter{
		cache:    cache,
		client:   client,
		ids:      ids,
		authors:  newRecordCache[*audnexusAuthor](authorCacheSize),
		ratings:  newRecordCache[*audibleRating](ratingCacheSize),
		products: newRecordCache[*audibleProduct](productCacheSize),
	}, nil
}

// searchLimit caps how many results we return for one query. Each result is
// cheap because Audible's search already includes the author ASIN, so no
// follow-up request is needed to build a SearchResource.
const searchLimit = 25

// Search queries Audible's catalog. An ASIN-shaped query is resolved directly
// rather than being passed to search, which lets the client's ASIN lookups
// short-circuit to an exact match.
func (g *ABGetter) Search(ctx context.Context, query string) ([]SearchResource, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.Join(errBadRequest, errors.New("empty query"))
	}

	if _asin.MatchString(strings.ToUpper(query)) {
		asin := strings.ToUpper(query)
		book, err := g.client.GetBook(ctx, asin)
		if err != nil {
			// Fall through to a keyword search: an ASIN that isn't in this
			// region may still match something by name.
			Log(ctx).Debug("ASIN lookup missed, falling back to search", "asin", asin, "err", err)
		} else {
			rsc, err := g.searchResource(ctx, book.ASIN, g.authorKeyOf(book.Authors), authorDisplayName(book.Authors))
			if err != nil {
				return nil, err
			}
			return []SearchResource{rsc}, nil
		}
	}

	products, err := g.client.SearchProducts(ctx, query, searchLimit)
	if err != nil {
		return nil, fmt.Errorf("searching: %w", err)
	}
	g.rememberRatings(ctx, products)

	// The client follows a search with /book/bulk over every result. Without
	// the listing cached, each of those costs its own audnexus request, so one
	// unidentified file during a library import spends ~26 requests instead of
	// one.
	g.rememberProducts(products)

	results := []SearchResource{}
	for _, p := range products {
		if p.ASIN == "" || len(p.Authors) == 0 {
			continue // Without an author the client can't do anything with it.
		}
		rsc, err := g.searchResource(ctx, p.ASIN, g.authorKeyOf(p.Authors), authorDisplayName(p.Authors))
		if err != nil {
			Log(ctx).Warn("problem mapping search result", "asin", p.ASIN, "err", err)
			continue
		}
		results = append(results, rsc)
	}

	return results, nil
}

// searchResource mints the three IDs a search result needs.
func (g *ABGetter) searchResource(ctx context.Context, bookASIN, authASIN, authName string) (SearchResource, error) {
	if authASIN == "" {
		return SearchResource{}, errors.Join(errNotFound, errors.New("missing author ASIN"))
	}

	workID, err := g.ids.ID(ctx, kindWork, bookASIN, "")
	if err != nil {
		return SearchResource{}, err
	}
	bookID, err := g.ids.ID(ctx, kindBook, bookASIN, "")
	if err != nil {
		return SearchResource{}, err
	}
	// Record the name as written. The key is normalized and lower cased, so
	// without this the only name available later is "david ludwig".
	authorID, err := g.ids.ID(ctx, kindAuthor, authASIN, cleanAuthorName(authName))
	if err != nil {
		return SearchResource{}, err
	}

	return SearchResource{
		BookID: bookID,
		WorkID: workID,
		Author: SearchResourceAuthor{ID: authorID},
	}, nil
}

// LookupASIN resolves an ASIN straight to an edition ID, no prior load needed.
//
// This is what makes the client's /book/asin/{asin} route work from a cold
// cache. The ASIN is verified against audnexus first so that a typo or an
// out-of-region product doesn't mint an ID that resolves to nothing.
func (g *ABGetter) LookupASIN(ctx context.Context, asin string) (int64, error) {
	book, err := g.client.GetBook(ctx, strings.ToUpper(strings.TrimSpace(asin)))
	if err != nil {
		return 0, fmt.Errorf("looking up %s: %w", asin, err)
	}
	return g.ids.ID(ctx, kindBook, book.ASIN, book.Title)
}

// GetWork returns the work for a surrogate work ID.
func (g *ABGetter) GetWork(ctx context.Context, workID int64, saveEditions editionsCallback) ([]byte, int64, error) {
	asin, err := g.ids.ASIN(ctx, kindWork, workID)
	if err != nil {
		return nil, 0, err
	}

	if cached, ttl, ok := g.cache.GetWithTTL(ctx, WorkKey(workID)); ok && ttl > 0 {
		return cached, 0, nil
	}

	Log(ctx).Debug("getting work", "workID", workID, "asin", asin)

	workRsc, err := g.workResource(ctx, asin)
	if err != nil {
		return nil, 0, err
	}

	// A work has exactly one edition here, so it can always be saved in the
	// same pass rather than costing another upstream request.
	if saveEditions != nil {
		saveEditions(workRsc)
	}

	out, err := json.Marshal(workRsc)
	if err != nil {
		return nil, 0, fmt.Errorf("marshaling work: %w", err)
	}

	return out, workRsc.Authors[0].ForeignID, nil
}

// GetBook returns the work containing the edition for a surrogate book ID.
//
// The editions callback is deliberately ignored. It runs GetAuthor to ensure
// the author is fetched, which can start a refresh, which fetches that
// author's books -- so calling it per book turns one fetch into a cascade of
// goroutines that each start another. A work here has exactly one edition and
// GetWork already saves it, so there is nothing for this to add. The Hardcover
// getter discards it for the same reason.
func (g *ABGetter) GetBook(ctx context.Context, bookID int64, _ editionsCallback) ([]byte, int64, int64, error) {
	asin, err := g.ids.ASIN(ctx, kindBook, bookID)
	if err != nil {
		return nil, 0, 0, err
	}

	if cached, ttl, ok := g.cache.GetWithTTL(ctx, BookKey(bookID)); ok && ttl > 0 {
		return cached, 0, 0, nil
	}

	Log(ctx).Debug("getting edition", "bookID", bookID, "asin", asin)

	workRsc, err := g.workResource(ctx, asin)
	if err != nil {
		return nil, 0, 0, err
	}

	out, err := json.Marshal(workRsc)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("marshaling work: %w", err)
	}

	return out, workRsc.ForeignID, workRsc.Authors[0].ForeignID, nil
}

// workResource maps an ASIN into the client's schema, preferring a catalog
// entry from an author walk over a fresh audnexus lookup.
//
// Refreshing one author otherwise costs a request per book -- hundreds for a
// prolific one, and the controller refreshes thirty authors at once -- which
// saturates the upstream budget and starves interactive requests. The listing
// that found those books already described them.
func (g *ABGetter) workResource(ctx context.Context, asin string) (workResource, error) {
	// A catalog listing describes a book well enough to identify and browse
	// it, and it's already in hand. Spending a request per book to improve a
	// description is what made an author refresh cost hundreds of them and a
	// library import spend tens of thousands.
	if p := g.product(asin); p != nil && !isDirectLookup(ctx) {
		return g.mapBook(ctx, p.asBook())
	}

	book, err := g.client.GetBook(ctx, asin)
	if err != nil {
		// A listing is better than nothing if the detail lookup fails.
		if p := g.product(asin); p != nil {
			Log(ctx).Debug("falling back to catalog data", "asin", asin, "err", err)
			return g.mapBook(ctx, p.asBook())
		}
		return workResource{}, fmt.Errorf("getting book %s: %w", asin, err)
	}

	// Full detail supersedes the listing, so later reads don't downgrade.
	g.forgetProduct(asin)

	return g.mapBook(ctx, book)
}

// directLookupKey marks a request for one specific book, as opposed to a bulk
// load or a background refresh.
type directLookupKey struct{}

// WithDirectLookup marks a request as asking for a single book by ID, which is
// what the client does when someone actually opens one. Those get full detail;
// everything else is served from the catalog listing that found it.
func WithDirectLookup(ctx context.Context) context.Context {
	return context.WithValue(ctx, directLookupKey{}, true)
}

func isDirectLookup(ctx context.Context) bool {
	direct, _ := ctx.Value(directLookupKey{}).(bool)
	return direct
}

// rememberRatings records rating detail from a catalog response. Ratings ride
// along on searches and author listings we already make, so no extra request
// is needed for the books a user actually sees.
func (g *ABGetter) rememberRatings(ctx context.Context, products []audibleProduct) {
	for _, p := range products {
		if p.ASIN == "" || p.Rating == nil {
			continue
		}
		g.ratings.Set(p.ASIN, p.Rating, 1)

		// Also keep it somewhere it can't be evicted. A rating is small and
		// changes slowly, and losing one is not the cheap miss the in-memory
		// caches are sized for: a book mapped without its rating goes out with
		// a count of zero, the client drops it for failing a minimum
		// popularity, and the zero is then cached with the work for weeks. One
		// eviction silently removes a book from a library.
		if encoded, err := json.Marshal(p.Rating); err == nil {
			g.cache.Set(ctx, ratingKey(p.ASIN), encoded, _ratingTTL)
		}
	}

	g.ratings.Wait()
}

// ratingKey returns the persistent cache key for an ASIN's rating.
func ratingKey(asin string) string {
	return "rt" + asin
}

// rating returns rating detail for an ASIN, from memory or the durable cache.
func (g *ABGetter) rating(ctx context.Context, asin string) *audibleRating {
	if r, ok := g.ratings.Get(asin); ok && r != nil {
		return r
	}

	encoded, ok := g.cache.Get(ctx, ratingKey(asin))
	if !ok {
		return nil
	}

	var r audibleRating
	if err := json.Unmarshal(encoded, &r); err != nil {
		return nil
	}

	g.ratings.Set(asin, &r, 1)

	return &r
}

// rememberProducts records catalog entries so an author's books can be mapped
// without a per-book request.
func (g *ABGetter) rememberProducts(products []audibleProduct) {
	for _, p := range products {
		if p.ASIN != "" {
			g.products.Set(p.ASIN, &p, 1)
		}
	}

	// Sets are buffered, and the bulk load that follows a search reads these
	// immediately -- without this it would miss and fetch each book instead.
	g.products.Wait()
}

// product returns a cached catalog entry for an ASIN, if one was seen.
func (g *ABGetter) product(asin string) *audibleProduct {
	p, _ := g.products.Get(asin)
	return p
}

// forgetProduct drops a cached listing once fuller detail has been fetched.
func (g *ABGetter) forgetProduct(asin string) {
	g.products.Del(asin)
}

// mapBook translates an audnexus book into the client's work schema.
func (g *ABGetter) mapBook(ctx context.Context, book *audnexusBook) (workResource, error) {
	primary, ok := primaryAuthor(book.Authors)
	if !ok {
		return workResource{}, errors.Join(errNotFound, fmt.Errorf("book %s has no author", book.ASIN))
	}
	authASIN := g.authorKeyOf(book.Authors)

	workID, err := g.ids.ID(ctx, kindWork, book.ASIN, book.Title)
	if err != nil {
		return workResource{}, err
	}
	bookID, err := g.ids.ID(ctx, kindBook, book.ASIN, book.Title)
	if err != nil {
		return workResource{}, err
	}
	authorID, err := g.ids.ID(ctx, kindAuthor, authASIN, cleanAuthorName(primary.Name))
	if err != nil {
		return workResource{}, err
	}

	series, err := g.mapSeries(ctx, book, workID)
	if err != nil {
		return workResource{}, err
	}

	genres := []string{}
	for _, genre := range book.Genres {
		genres = append(genres, genre.Name)
	}
	if len(genres) == 0 {
		genres = []string{"none"}
	}

	description := book.Description
	if description == "" {
		description = book.Summary
	}
	if description == "" {
		description = "N/A" // Must be set.
	}

	title := book.Title
	fullTitle := title
	if book.Subtitle != "" {
		fullTitle = title + ": " + book.Subtitle
	}

	released, releasedRaw := audibleReleaseDate(book.ReleaseDate)

	// audnexus gives a display average and nothing else. Audible's catalog has
	// the counts, which the client needs to order search results.
	rating, _ := strconv.ParseFloat(book.Rating, 64)
	ratingCount := int64(0)
	if r := g.rating(ctx, book.ASIN); r != nil {
		ratingCount = r.OverallDistribution.NumRatings
		if r.OverallDistribution.AverageRating > 0 {
			rating = r.OverallDistribution.AverageRating
		}
	}
	ratingSum := int64(float64(ratingCount) * rating)

	editionInfo := ""
	if narrators := personNames(book.Narrators); len(narrators) > 0 {
		// There's no narrator field in the client's schema, but edition info
		// is surfaced in the UI and is the main thing distinguishing one
		// Audible edition from another.
		editionInfo = "Narrated by " + strings.Join(narrators, ", ")
	}

	bookRsc := bookResource{
		ForeignID:          bookID,
		Asin:               book.ASIN,
		Description:        description,
		Isbn13:             book.ISBN,
		Title:              title,
		FullTitle:          fullTitle,
		ShortTitle:         title,
		Language:           normalizeLanguage(book.Language),
		Format:             "Audiobook",
		EditionInformation: editionInfo,
		Publisher:          book.PublisherName,
		ImageURL:           book.Image,
		IsEbook:            false, // Audible is audio-only by definition.
		NumPages:           0,     // Audible reports runtime, not pages.
		RatingCount:        ratingCount,
		RatingSum:          ratingSum,
		AverageRating:      rating,
		URL:                audibleURL(book.ASIN),
		ReleaseDate:        released,
		ReleaseDateRaw:     releasedRaw,
		Contributors:       []contributorResource{{ForeignID: authorID, Role: "Author"}},
	}

	authorRsc := AuthorResource{
		ForeignID:   authorID,
		Name:        cleanAuthorName(primary.Name),
		Description: "N/A", // Must be set.
		URL:         authorURL(authASIN),
		Series:      series,
	}

	// The client renders the author embedded in a work, so search results show
	// this rather than whatever GetAuthor later returns. Leaving it as a stub
	// makes every author look empty in search.
	if detail := g.authorDetailByName(ctx, authASIN, cleanAuthorName(primary.Name)); detail != nil {
		if detail.Name != "" {
			authorRsc.Name = detail.Name
		}
		if detail.Description != "" {
			authorRsc.Description = detail.Description
		}
		authorRsc.ImageURL = detail.Image
	}

	workRsc := workResource{
		ForeignID:      workID,
		Title:          title,
		FullTitle:      fullTitle,
		ShortTitle:     title,
		URL:            audibleURL(book.ASIN),
		ReleaseDate:    released,
		ReleaseDateRaw: releasedRaw,
		Genres:         genres,
		RelatedWorks:   []int{},
		Series:         series,
		BestBookID:     bookID,
		RatingCount:    ratingCount,
		RatingSum:      ratingSum,
		AverageRating:  rating,
	}

	authorRsc.Works = []workResource{workRsc}
	workRsc.Authors = []AuthorResource{authorRsc}
	workRsc.Books = []bookResource{bookRsc}

	return workRsc, nil
}

// mapSeries builds series links for a book. Audible gives a position directly,
// so no inference is needed. The series title is recorded with its ID because
// Audible offers no way to resolve a series ASIN back to a name later.
func (g *ABGetter) mapSeries(ctx context.Context, book *audnexusBook, workID int64) ([]SeriesResource, error) {
	series := []SeriesResource{}

	for _, ref := range []*audnexusSeriesRef{book.SeriesPrimary, book.SeriesSecondary} {
		if ref == nil || ref.ASIN == "" {
			continue
		}

		seriesID, err := g.ids.ID(ctx, kindSeries, ref.ASIN, ref.Name)
		if err != nil {
			return nil, err
		}

		position, _ := strconv.Atoi(strings.TrimSpace(ref.Position))

		series = append(series, SeriesResource{
			ForeignID: seriesID,
			Title:     ref.Name,
			LinkItems: []seriesWorkLinkResource{{
				ForeignWorkID:    workID,
				PositionInSeries: ref.Position,
				SeriesPosition:   position,
				Primary:          ref == book.SeriesPrimary,
			}},
		})
	}

	return series, nil
}

// authorKeyOf returns the mapping key for a book's primary author.
//
// Every path that mints an author ID has to agree. Search minting a
// name-derived key while the work it points at maps to an aliased ASIN gives
// the client an ID that GetAuthor can find no works for, which surfaces as
// "an author with this ID was not found".
func (g *ABGetter) authorKeyOf(authors []audnexusPerson) string {
	primary, ok := primaryAuthor(authors)
	if !ok {
		return ""
	}
	return authorKey(primary)
}

// isBackground reports whether this context belongs to a refresh or
// denormalization rather than something a user is waiting on. The controller
// tags those with its own request IDs.
func isBackground(ctx context.Context) bool {
	id := middleware.GetReqID(ctx)

	return strings.HasPrefix(id, "refresh-") ||
		strings.HasPrefix(id, "denorm-") ||
		strings.HasPrefix(id, "save-editions-")
}

// authorIdentity returns the display name for an author key plus, when
// audnexus can be matched to it, their bio and photo.
//
// The lookup is enrichment only. Identity is already settled by the key, so a
// miss costs a picture rather than changing which author this is.
func (g *ABGetter) authorIdentity(ctx context.Context, key, label string) (string, *audnexusAuthor) {
	name := label
	if name == "" {
		normalized, _ := authorName(key)
		name = titleCaseName(normalized)
	}
	if name == "" {
		return "", nil
	}

	detail := g.authorDetailByName(ctx, key, name)
	if detail != nil && detail.Name != "" {
		// audnexus has the author as they're actually written.
		name = cleanAuthorName(detail.Name)
	}
	return name, detail
}

// authorDetailByName finds an author's audnexus record by name, once per key.
//
// Audible lists the same person under several ASINs and audnexus keeps a
// record per ASIN, so an exact name match is not unique -- "George Saunders"
// returns two, one with a bio and photo and one with neither. Taking the first
// leaves an author looking blank when their details were a request away.
func (g *ABGetter) authorDetailByName(ctx context.Context, key, name string) *audnexusAuthor {
	if detail, ok := g.authors.Get(key); ok {
		return detail
	}

	// Enrichment is a picture and a bio, and it costs a name search plus up to
	// three record fetches. During an import that is thousands of authors the
	// user may never look at, and audnexus starts refusing the traffic. Do it
	// when someone is actually looking, not while walking catalogs in the
	// background.
	if isBackground(ctx) {
		return nil
	}

	candidates, err := g.client.SearchAuthors(ctx, name)
	if err != nil {
		// A failure isn't the same answer as "no such author", so it isn't
		// remembered for good -- but not remembering it at all means retrying
		// for every book by that author. Under a rate limit that turns one
		// refused request into a storm that sustains itself: each retry adds
		// load, the load causes more refusals, and every attempt holds a
		// goroutine through its backoff. Remember it briefly instead.
		Log(ctx).Debug("author search failed", "name", name, "err", err)
		g.authors.SetWithTTL(key, nil, 1, authorFailureTTL)
		g.authors.Wait()

		return nil
	}

	detail := g.bestAuthorRecord(ctx, candidates, name)

	// Nothing matched is a real answer and worth remembering.
	g.authors.Set(key, detail, 1)
	g.authors.Wait()

	return detail
}

// authorRecordCandidates bounds how many records are fetched for one name.
// Exact matches are usually one or two; anything beyond that is a name common
// enough that more requests won't settle it.
const authorRecordCandidates = 3

// bestAuthorRecord combines the exact name matches into one record.
//
// The fullest match is the base, and anything it's missing is filled from the
// others. Audible splits one person across several ASINs and audnexus keeps a
// record per ASIN, so the bio and the photo are routinely on different ones --
// picking either alone shows an author as half blank when both were available.
func (g *ABGetter) bestAuthorRecord(ctx context.Context, candidates []audnexusAuthor, name string) *audnexusAuthor {
	// The search is fuzzy -- "George Saunders" also returns "George Shipway"
	// and "Scott George" -- so only an exact match on the normalized name
	// counts.
	want := normalizeAuthorName(name)

	records := []*audnexusAuthor{}
	seen := map[string]bool{}

	for _, c := range candidates {
		if c.ASIN == "" || seen[c.ASIN] || normalizeAuthorName(c.Name) != want {
			continue
		}
		seen[c.ASIN] = true

		if len(records) >= authorRecordCandidates {
			break
		}

		full, err := g.client.GetAuthor(ctx, c.ASIN)
		if err != nil {
			continue
		}
		records = append(records, full)
	}

	if len(records) == 0 {
		return nil
	}

	// Start from the fullest so its identity and its longer text win, then
	// take whatever it lacks from the rest.
	best := 0
	for i, r := range records {
		if authorRecordScore(r) > authorRecordScore(records[best]) {
			best = i
		}
	}

	merged := *records[best]

	for i, r := range records {
		if i == best {
			continue
		}
		fillAuthorGaps(&merged, r)
	}

	return &merged
}

// fillAuthorGaps copies anything dst is missing from src.
func fillAuthorGaps(dst *audnexusAuthor, src *audnexusAuthor) {
	if dst.Description == "" {
		dst.Description = src.Description
	}
	if dst.Image == "" {
		dst.Image = src.Image
	}
	if len(dst.Genres) == 0 {
		dst.Genres = src.Genres
	}
	if dst.Name == "" {
		dst.Name = src.Name
	}
	if dst.ASIN == "" {
		dst.ASIN = src.ASIN
	}
}

// authorRecordScore ranks how much an audnexus record actually says.
func authorRecordScore(a *audnexusAuthor) int {
	score := 0
	if a.Description != "" {
		score += 2
	}
	if a.Image != "" {
		score++
	}
	return score
}

// GetAuthor returns an author seeded with one of their works.
func (g *ABGetter) GetAuthor(ctx context.Context, authorID int64) ([]byte, error) {
	ref, err := g.ids.Ref(ctx, authorID)
	if err != nil {
		return nil, err
	}
	if ref.kind != kindAuthor {
		return nil, errors.Join(errBadRequest, fmt.Errorf("ID %d is not an author", authorID))
	}
	asin := ref.asin

	Log(ctx).Debug("getting author", "authorID", authorID, "asin", asin)

	name, detail := g.authorIdentity(ctx, asin, ref.label)
	if name == "" {
		return nil, errors.Join(errNotFound, fmt.Errorf("no detail for author %s", asin))
	}

	products, _, err := g.client.ProductsByAuthor(ctx, name, 0)
	if err != nil {
		return nil, fmt.Errorf("getting author products: %w", err)
	}
	g.rememberRatings(ctx, products)

	description := "N/A" // Must be set.
	image := ""
	if detail != nil {
		if detail.Description != "" {
			description = detail.Description
		}
		image = detail.Image
	}

	// Seed the author with the first work we can actually load. The controller
	// backfills the rest from GetAuthorBooks.
	for _, p := range products {
		// Seed with a book this author leads, for the same reason the walk
		// only yields those.
		if g.authorKeyOf(p.Authors) != asin {
			continue
		}

		work, err := g.workResource(ctx, p.ASIN)
		if err != nil {
			Log(ctx).Debug("skipping unloadable work for author", "asin", p.ASIN, "err", err)
			continue
		}

		// mapBook credits a work to its primary author, which on a
		// co-authored book may not be the author being requested. Adopting it
		// anyway would return this author's name and bio attached to another
		// author's ID and bibliography.
		if len(work.Authors) == 0 || work.Authors[0].ForeignID != authorID {
			Log(ctx).Debug("skipping work credited to another author",
				"asin", p.ASIN, "authorID", authorID)
			continue
		}

		authorRsc := work.Authors[0]
		authorRsc.Name = name
		authorRsc.Description = description
		authorRsc.ImageURL = image
		authorRsc.URL = authorURL(asin)
		authorRsc.Works = []workResource{work}

		return json.Marshal(authorRsc)
	}

	Log(ctx).Warn("no valid works found", "authorID", authorID, "asin", asin)
	return nil, errors.Join(errNotFound, fmt.Errorf("no valid works for author %s", asin))
}

// GetAuthorBooks yields every edition ID credited to an author.
//
// Audible only filters the catalog by author name, so results are checked
// against the author's ASIN, which the response includes. That keeps authors
// who share a name from bleeding into each other.
func (g *ABGetter) GetAuthorBooks(ctx context.Context, authorID int64) iter.Seq[int64] {
	return func(yield func(int64) bool) {
		ref, err := g.ids.Ref(ctx, authorID)
		if err != nil || ref.kind != kindAuthor {
			Log(ctx).Warn("unknown author ID", "authorID", authorID, "err", err)
			return
		}
		asin := ref.asin

		name, _ := g.authorIdentity(ctx, asin, ref.label)
		if name == "" {
			Log(ctx).Warn("no identity for author", "asin", asin)
			return
		}

		seen := map[string]bool{}

		// Audible's page parameter is 0-indexed: page=1 skips the first
		// num_results items. Starting at 1 silently drops an author's first
		// page of books, and returns nothing at all for authors with fewer
		// than num_results titles.
		for page := 0; ; page++ {
			products, total, err := g.client.ProductsByAuthor(ctx, name, page)
			if err != nil {
				Log(ctx).Warn("problem getting author products", "asin", asin, "page", page, "err", err)
				return
			}
			if len(products) == 0 {
				return
			}
			g.rememberRatings(ctx, products)
			g.rememberProducts(products)

			for _, p := range products {
				// Only books this author leads. A book they merely contributed
				// to belongs to its primary author, and the client discards it
				// from this author anyway -- but not before fetching it,
				// minting an author for whoever does lead it, and queueing
				// that author's own catalog to be walked. With Audible's
				// anthologies and collaborations that expands across the
				// co-authorship graph until it has walked most of the store.
				if p.ASIN == "" || seen[p.ASIN] || g.authorKeyOf(p.Authors) != asin {
					continue
				}
				seen[p.ASIN] = true

				bookID, err := g.ids.ID(ctx, kindBook, p.ASIN, p.Title)
				if err != nil {
					Log(ctx).Warn("problem mapping book", "asin", p.ASIN, "err", err)
					continue
				}
				if !yield(bookID) {
					return
				}
			}

			if (page+1)*audiblePageSize >= total {
				return
			}
		}
	}
}

// GetSeries returns the works in a series.
//
// Audible has no endpoint for this: /catalog/series/{asin} doesn't exist and
// the catalog's series_asin parameter is silently ignored. The series title is
// therefore searched and the results filtered by series ASIN, which is exact
// for everything the search surfaces but will miss entries it ranks poorly.
func (g *ABGetter) GetSeries(ctx context.Context, seriesID int64) (*SeriesResource, error) {
	ref, err := g.ids.Ref(ctx, seriesID)
	if err != nil {
		return nil, err
	}
	if ref.kind != kindSeries {
		return nil, errors.Join(errBadRequest, fmt.Errorf("ID %d is not a series", seriesID))
	}
	if ref.label == "" {
		return nil, errors.Join(errNotFound, fmt.Errorf("no title recorded for series %s", ref.asin))
	}

	products, err := g.client.SearchProducts(ctx, ref.label, audiblePageSize)
	if err != nil {
		return nil, fmt.Errorf("getting series %d: %w", seriesID, err)
	}

	seriesRsc := &SeriesResource{
		ForeignID: seriesID,
		Title:     ref.label,
		LinkItems: []seriesWorkLinkResource{},
	}

	seen := map[int64]bool{}

	for _, p := range products {
		for _, s := range p.Series {
			if !strings.EqualFold(s.ASIN, ref.asin) {
				continue
			}

			workID, err := g.ids.ID(ctx, kindWork, p.ASIN, p.Title)
			if err != nil {
				Log(ctx).Warn("problem mapping series work", "asin", p.ASIN, "err", err)
				continue
			}
			if seen[workID] {
				continue
			}
			seen[workID] = true

			position, _ := strconv.Atoi(strings.TrimSpace(s.Sequence))
			seriesRsc.LinkItems = append(seriesRsc.LinkItems, seriesWorkLinkResource{
				ForeignWorkID:    workID,
				PositionInSeries: s.Sequence,
				SeriesPosition:   position,
				Primary:          true,
			})
		}
	}

	return seriesRsc, nil
}

// Recommendations isn't implemented. Audible has no unauthenticated trending
// or recommendations endpoint, and an empty list is more useful to the client
// than an error on a non-essential route.
func (g *ABGetter) Recommendations(ctx context.Context, page int64) (RecommentationsResource, error) {
	if page < 1 {
		return RecommentationsResource{}, fmt.Errorf("page must be gte 1")
	}
	return RecommentationsResource{WorkIDs: []int64{}}, nil
}

// primaryAuthor returns the first credited author that has an ASIN.
//
// Both the name and the ASIN must come from the same entry. Audible credits
// co-authors and contributors, and some of them have no ASIN, so taking the
// name from the first entry and the ASIN from wherever one happens to appear
// builds an identity out of two different people -- one author's name against
// another author's ID and bibliography.
func primaryAuthor(authors []audnexusPerson) (audnexusPerson, bool) {
	// Prefer a credit that isn't an editor, translator or similar. Audible
	// appends the role to the name ("Shawn Speakman - editor"), and anthologies
	// often list one first, which would otherwise make every editor an author
	// with a bibliography of their own to walk.
	// A credit carrying a real ASIN is the strongest signal, so an anthology
	// that lists an uncredited contributor first still resolves to the author
	// Audible actually identifies.
	for _, a := range authors {
		if a.ASIN != "" && !_contributorRole.MatchString(a.Name) {
			return a, true
		}
	}
	for _, a := range authors {
		if authorKey(a) != "" && !_contributorRole.MatchString(a.Name) {
			return a, true
		}
	}

	// A book credited only to an editor still needs someone to hang it on.
	for _, a := range authors {
		if a.ASIN != "" {
			return a, true
		}
	}
	for _, a := range authors {
		if authorKey(a) != "" {
			return a, true
		}
	}

	return audnexusPerson{}, false
}

// authorKey returns the mapping key for an author.
//
// Identity is the author's normalized name, never their ASIN. Audible omits
// the ASIN on roughly a third of titles, lists the same person under several,
// and credits them under varying names -- "David Ludwig", "David Ludwig MD
// PhD", even "David Ludwig MD PhD MD PhD" -- so anything keyed on ASINs
// either drops books or splits one author across entries. Keying on the name
// is deterministic and order-independent, which an ASIN cache resolved as we
// went could never be.
//
// Two different authors who share a name therefore merge. That is the
// deliberate trade: one entry holding everything beats hunting the same
// author's books across several.
func authorKey(a audnexusPerson) string {
	name := normalizeAuthorName(a.Name)
	if name == "" {
		return ""
	}
	return _authorNameKey + name
}

// _authorNameKey prefixes a name-derived key so it can't be mistaken for an
// ASIN and so the lookup path can tell the two apart.
const _authorNameKey = "name:"

// _authorCredentials matches the honorifics and post-nominals Audible appends
// to a credit, which vary between titles by the same author.
var _authorCredentials = regexp.MustCompile(
	`(?i)[,\s]+(m\.?d\.?|ph\.?d\.?|psy\.?d\.?|ed\.?d\.?|d\.?o\.?|d\.?d\.?s\.?|r\.?n\.?|m\.?b\.?a\.?|m\.?s\.?w?\.?|m\.?a\.?|j\.?d\.?|l\.?c\.?s\.?w\.?|esq\.?|jr\.?|sr\.?|i{2,3}|iv)\.?$`)

// cleanAuthorName strips the role and credentials Audible appends, keeping the
// name as written otherwise so it stays presentable.
func cleanAuthorName(raw string) string {
	name := strings.TrimSpace(_contributorRole.ReplaceAllString(strings.TrimSpace(raw), ""))

	// Repeated because credentials stack, and Audible sometimes repeats them.
	for {
		stripped := strings.TrimSpace(_authorCredentials.ReplaceAllString(name, ""))
		if stripped == name || stripped == "" {
			break
		}
		name = stripped
	}

	return strings.Join(strings.Fields(name), " ")
}

// normalizeAuthorName reduces a credit to the form used for identity.
func normalizeAuthorName(raw string) string {
	return strings.ToLower(cleanAuthorName(raw))
}

// authorName recovers the name from a name-derived key.
func authorName(key string) (string, bool) {
	name, ok := strings.CutPrefix(key, _authorNameKey)
	return name, ok
}

// _contributorRole matches the role Audible appends to a non-author credit.
var _contributorRole = regexp.MustCompile(
	`(?i) - (editor|translator|adaptation|adapted by|introduction|contributor|foreword|afterword|illustrator|narrator|preface|compiler|annotations?|notes)$`)

// authorASIN returns the mapping key for a book's primary author.
func authorASIN(authors []audnexusPerson) string {
	a, _ := primaryAuthor(authors)
	return authorKey(a)
}

// authorDisplayName returns the primary author's name as written.
func authorDisplayName(authors []audnexusPerson) string {
	primary, ok := primaryAuthor(authors)
	if !ok {
		return ""
	}
	return cleanAuthorName(primary.Name)
}

// titleCaseName restores presentable casing for a name recovered from a key.
// Keys are lower cased for identity, so this is the last resort when nothing
// recorded how the author is actually written.
func titleCaseName(name string) string {
	return cases.Title(language.English).String(name)
}

// authorURL links to an author's Audible page, or to a search for them when
// there's no ASIN to link to.
//
// This must never be empty. The client only records a link when the URL is
// non-empty, and its add-author view reads links[0].url without checking, so
// an author with no link crashes the page.
func authorURL(key string) string {
	if name, isName := authorName(key); isName {
		return "https://www.audible.com/search?keywords=" + url.QueryEscape(name)
	}
	return audibleURL(key)
}

func personNames(people []audnexusPerson) []string {
	names := []string{}
	for _, p := range people {
		if p.Name != "" {
			names = append(names, p.Name)
		}
	}
	return names
}

func audibleURL(asin string) string {
	return "https://www.audible.com/pd/" + asin
}

// audibleReleaseDate normalizes audnexus timestamps to the date-only format
// the client parses, returning both it and the original value.
func audibleReleaseDate(raw string) (string, string) {
	if raw == "" {
		return "", ""
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05.000Z", time.DateOnly} {
		t, err := time.Parse(layout, raw)
		if err != nil {
			continue
		}
		if t.Year() < 1 || t.Year() > 9999 {
			return "", raw
		}
		return t.Format(time.DateOnly), raw
	}
	return "", raw
}

// normalizeLanguage maps Audible's language names onto the three-letter codes
// the other upstreams emit, so language-based quality profiles behave the same
// way regardless of metadata source.
func normalizeLanguage(lang string) string {
	switch strings.ToLower(strings.TrimSpace(lang)) {
	case "english":
		return "eng"
	case "spanish", "castilian":
		return "spa"
	case "french":
		return "fre"
	case "german":
		return "ger"
	case "italian":
		return "ita"
	case "portuguese":
		return "por"
	case "dutch":
		return "dut"
	case "japanese":
		return "jpn"
	case "russian":
		return "rus"
	case "chinese", "mandarin_chinese":
		return "chi"
	case "":
		return ""
	default:
		return strings.ToLower(lang)
	}
}
