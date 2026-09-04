package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strconv"
	"strings"
	"sync"
	"time"
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

	// authors caches author detail so mapping a book doesn't cost an upstream
	// request per contributor -- a bulk load pulls many books by one author,
	// and getters aren't allowed to write to the shared cache. Bounded by the
	// number of distinct authors in a library, which is small.
	authorMu sync.RWMutex
	authors  map[string]*audnexusAuthor

	// ratings caches rating counts seen in catalog responses. audnexus omits
	// them, and the client sorts search results by rating count -- with every
	// count equal the sort is comparing a constant and the order it produces
	// is arbitrary.
	ratingMu sync.RWMutex
	ratings  map[string]*audibleRating
}

var (
	_ getter     = (*ABGetter)(nil)
	_ asinLookup = (*ABGetter)(nil)
)

// NewAudibleGetter returns a new getter backed by Audible.
func NewAudibleGetter(cache cache[[]byte], client *AudibleClient, ids *IDMapper) (*ABGetter, error) {
	return &ABGetter{
		cache:   cache,
		client:  client,
		ids:     ids,
		authors: map[string]*audnexusAuthor{},
		ratings: map[string]*audibleRating{},
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
			rsc, err := g.searchResource(ctx, book.ASIN, authorASIN(book.Authors))
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
	g.rememberRatings(products)

	results := []SearchResource{}
	for _, p := range products {
		if p.ASIN == "" || len(p.Authors) == 0 {
			continue // Without an author the client can't do anything with it.
		}
		rsc, err := g.searchResource(ctx, p.ASIN, authorASIN(p.Authors))
		if err != nil {
			Log(ctx).Warn("problem mapping search result", "asin", p.ASIN, "err", err)
			continue
		}
		results = append(results, rsc)
	}

	return results, nil
}

// searchResource mints the three IDs a search result needs.
func (g *ABGetter) searchResource(ctx context.Context, bookASIN, authASIN string) (SearchResource, error) {
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
	authorID, err := g.ids.ID(ctx, kindAuthor, authASIN, "")
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
func (g *ABGetter) GetBook(ctx context.Context, bookID int64, saveEditions editionsCallback) ([]byte, int64, int64, error) {
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

	if saveEditions != nil {
		saveEditions(workRsc)
	}

	out, err := json.Marshal(workRsc)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("marshaling work: %w", err)
	}

	return out, workRsc.ForeignID, workRsc.Authors[0].ForeignID, nil
}

// workResource fetches an ASIN and maps it into the client's schema.
func (g *ABGetter) workResource(ctx context.Context, asin string) (workResource, error) {
	book, err := g.client.GetBook(ctx, asin)
	if err != nil {
		return workResource{}, fmt.Errorf("getting book %s: %w", asin, err)
	}
	return g.mapBook(ctx, book)
}

// rememberRatings records rating detail from a catalog response. Ratings ride
// along on searches and author listings we already make, so no extra request
// is needed for the books a user actually sees.
func (g *ABGetter) rememberRatings(products []audibleProduct) {
	g.ratingMu.Lock()
	defer g.ratingMu.Unlock()

	for _, p := range products {
		if p.ASIN != "" && p.Rating != nil {
			g.ratings[p.ASIN] = p.Rating
		}
	}
}

// rating returns cached rating detail for an ASIN, if it's been seen.
func (g *ABGetter) rating(asin string) *audibleRating {
	g.ratingMu.RLock()
	defer g.ratingMu.RUnlock()
	return g.ratings[asin]
}

// authorDetail returns an author's name, image and bio, fetching each ASIN at
// most once. A failed lookup is cached as nil so a book by an author audnexus
// doesn't know about doesn't retry on every subsequent mapping.
func (g *ABGetter) authorDetail(ctx context.Context, asin string) *audnexusAuthor {
	g.authorMu.RLock()
	author, ok := g.authors[asin]
	g.authorMu.RUnlock()
	if ok {
		return author
	}

	author, err := g.client.GetAuthor(ctx, asin)
	if err != nil {
		Log(ctx).Debug("no author detail", "asin", asin, "err", err)
		author = nil
	}

	g.authorMu.Lock()
	g.authors[asin] = author
	g.authorMu.Unlock()

	return author
}

// mapBook translates an audnexus book into the client's work schema.
func (g *ABGetter) mapBook(ctx context.Context, book *audnexusBook) (workResource, error) {
	authASIN := authorASIN(book.Authors)
	if authASIN == "" {
		return workResource{}, errors.Join(errNotFound, fmt.Errorf("book %s has no author", book.ASIN))
	}

	workID, err := g.ids.ID(ctx, kindWork, book.ASIN, book.Title)
	if err != nil {
		return workResource{}, err
	}
	bookID, err := g.ids.ID(ctx, kindBook, book.ASIN, book.Title)
	if err != nil {
		return workResource{}, err
	}
	authorID, err := g.ids.ID(ctx, kindAuthor, authASIN, book.Authors[0].Name)
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
	if r := g.rating(book.ASIN); r != nil {
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
		Name:        book.Authors[0].Name,
		Description: "N/A", // Must be set.
		URL:         audibleURL(authASIN),
		Series:      series,
	}

	// The client renders the author embedded in a work, so search results show
	// this rather than whatever GetAuthor later returns. Leaving it as a stub
	// makes every author look empty in search.
	if detail := g.authorDetail(ctx, authASIN); detail != nil {
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

// GetAuthor returns an author seeded with one of their works.
func (g *ABGetter) GetAuthor(ctx context.Context, authorID int64) ([]byte, error) {
	asin, err := g.ids.ASIN(ctx, kindAuthor, authorID)
	if err != nil {
		return nil, err
	}

	Log(ctx).Debug("getting author", "authorID", authorID, "asin", asin)

	author := g.authorDetail(ctx, asin)
	if author == nil {
		return nil, errors.Join(errNotFound, fmt.Errorf("no detail for author %s", asin))
	}

	products, _, err := g.client.ProductsByAuthor(ctx, author.Name, 0)
	if err != nil {
		return nil, fmt.Errorf("getting author products: %w", err)
	}
	g.rememberRatings(products)

	description := author.Description
	if description == "" {
		description = "N/A" // Must be set.
	}

	// Seed the author with the first work we can actually load. The controller
	// backfills the rest from GetAuthorBooks.
	for _, p := range products {
		if !p.hasAuthor(asin) {
			continue
		}

		work, err := g.workResource(ctx, p.ASIN)
		if err != nil {
			Log(ctx).Debug("skipping unloadable work for author", "asin", p.ASIN, "err", err)
			continue
		}

		authorRsc := work.Authors[0]
		authorRsc.Name = author.Name
		authorRsc.Description = description
		authorRsc.ImageURL = author.Image
		authorRsc.URL = audibleURL(asin)
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
		asin, err := g.ids.ASIN(ctx, kindAuthor, authorID)
		if err != nil {
			Log(ctx).Warn("unknown author ID", "authorID", authorID, "err", err)
			return
		}

		author, err := g.client.GetAuthor(ctx, asin)
		if err != nil {
			Log(ctx).Warn("problem getting author", "asin", asin, "err", err)
			return
		}

		seen := map[string]bool{}

		// Audible's page parameter is 0-indexed: page=1 skips the first
		// num_results items. Starting at 1 silently drops an author's first
		// page of books, and returns nothing at all for authors with fewer
		// than num_results titles.
		for page := 0; ; page++ {
			products, total, err := g.client.ProductsByAuthor(ctx, author.Name, page)
			if err != nil {
				Log(ctx).Warn("problem getting author products", "asin", asin, "page", page, "err", err)
				return
			}
			if len(products) == 0 {
				return
			}
			g.rememberRatings(products)

			for _, p := range products {
				if p.ASIN == "" || seen[p.ASIN] || !p.hasAuthor(asin) {
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

// authorASIN returns the first author with an ASIN. Audible lists co-authors
// and occasionally credits contributors without ASINs.
func authorASIN(authors []audnexusPerson) string {
	for _, a := range authors {
		if a.ASIN != "" {
			return a.ASIN
		}
	}
	return ""
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
