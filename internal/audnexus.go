package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Audible splits its catalog by region and audnexus follows suit, so every
// lookup is scoped to one marketplace. Mixing regions produces ASINs the
// client can't resolve later, so the region is fixed per-server rather than
// per-request.
const defaultRegion = "us"

// audnexusBook is the normalized book audnexus returns for an ASIN. This is
// the same shape Audiobookshelf consumes, which is the point: matching stays
// consistent between the two.
type audnexusBook struct {
	ASIN             string             `json:"asin"`
	Authors          []audnexusPerson   `json:"authors"`
	Narrators        []audnexusPerson   `json:"narrators"`
	Description      string             `json:"description"`
	Summary          string             `json:"summary"`
	FormatType       string             `json:"formatType"`
	Genres           []audnexusGenre    `json:"genres"`
	Image            string             `json:"image"`
	ISBN             string             `json:"isbn"`
	Language         string             `json:"language"`
	LiteratureType   string             `json:"literatureType"`
	PublisherName    string             `json:"publisherName"`
	Rating           string             `json:"rating"`
	Region           string             `json:"region"`
	ReleaseDate      string             `json:"releaseDate"`
	RuntimeLengthMin int64              `json:"runtimeLengthMin"`
	SeriesPrimary    *audnexusSeriesRef `json:"seriesPrimary"`
	SeriesSecondary  *audnexusSeriesRef `json:"seriesSecondary"`
	Subtitle         string             `json:"subtitle"`
	Title            string             `json:"title"`
}

type audnexusPerson struct {
	ASIN string `json:"asin"`
	Name string `json:"name"`
}

type audnexusGenre struct {
	ASIN string `json:"asin"`
	Name string `json:"name"`
	Type string `json:"type"`
}

type audnexusSeriesRef struct {
	ASIN     string `json:"asin"`
	Name     string `json:"name"`
	Position string `json:"position"`
}

// audnexusAuthor is the author detail audnexus returns for an ASIN.
type audnexusAuthor struct {
	ASIN        string          `json:"asin"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Image       string          `json:"image"`
	Genres      []audnexusGenre `json:"genres"`
}

// audibleProduct is one product from Audible's catalog API. Only the fields we
// map are declared; the full response is very large.
type audibleProduct struct {
	ASIN                 string            `json:"asin"`
	Title                string            `json:"title"`
	Subtitle             string            `json:"subtitle"`
	Authors              []audnexusPerson  `json:"authors"`
	Narrators            []audnexusPerson  `json:"narrators"`
	MerchandisingSummary string            `json:"merchandising_summary"`
	PublisherName        string            `json:"publisher_name"`
	ReleaseDate          string            `json:"release_date"`
	Language             string            `json:"language"`
	RuntimeLengthMin     int64             `json:"runtime_length_min"`
	Series               []audibleSeries   `json:"series"`
	Rating               *audibleRating    `json:"rating"`
	ProductImages        map[string]string `json:"product_images"`
}

// audibleRating is Audible's rating detail. audnexus exposes an average but no
// count, and the client sorts search results by rating count, so this is the
// only source for that ordering.
type audibleRating struct {
	NumReviews          int64 `json:"num_reviews"`
	OverallDistribution struct {
		AverageRating float64 `json:"average_rating"`
		NumRatings    int64   `json:"num_ratings"`
	} `json:"overall_distribution"`
}

type audibleSeries struct {
	ASIN     string `json:"asin"`
	Title    string `json:"title"`
	Sequence string `json:"sequence"`
}

type audibleProducts struct {
	Products     []audibleProduct `json:"products"`
	TotalResults int              `json:"total_results"`
}

// AudibleClient talks to both audnexus and Audible's catalog API.
//
// The split is not a preference: audnexus has no book search — it only
// resolves ASINs (/books/{asin}) and author names (/authors?name=) — while
// Audible's catalog API is the only one of the two that answers natural
// language queries. Detail lookups go to audnexus because its output is
// already normalized and cached, and because it's the same data
// Audiobookshelf shows.
type AudibleClient struct {
	audnexus *http.Client
	audible  *http.Client

	audnexusHost string
	audibleHost  string
	region       string
}

// NewAudibleClient returns a client for the given hosts and region. rate is the
// per-upstream request ceiling in requests per second; zero uses the default.
func NewAudibleClient(audnexusHost, audibleHost, region string, rate int) *AudibleClient {
	if region == "" {
		region = defaultRegion
	}
	if rate <= 0 {
		rate = defaultRate
	}
	return &AudibleClient{
		audnexus:     newThrottledClient(rate),
		audible:      newThrottledClient(rate),
		audnexusHost: audnexusHost,
		audibleHost:  audibleHost,
		region:       region,
	}
}

// defaultRate is the per-upstream requests-per-second ceiling.
//
// The controller runs up to 30 author refreshes and 25 work refreshes at once,
// and unlike the Hardcover upstream -- which batches 25 GraphQL queries into a
// single request -- every book here costs its own HTTP call. At the 3/s the
// other upstreams use, an interactive request queues behind the whole
// background backlog and takes ten seconds or more to come back.
const defaultRate = 10

// newThrottledClient rate limits an unauthenticated upstream. Neither API
// documents a limit and both will start refusing traffic if pushed, so this is
// tunable: raise it to import a large library faster, lower it if audnexus --
// a free community service -- starts returning 429s.
func newThrottledClient(rate int) *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: throttledTransport{
			ticker: time.NewTicker(time.Second / time.Duration(rate)),
			// Deliberately not errorProxyTransport: it turns any >=400 into an
			// error and drops the response, which would hide both the status
			// and the Retry-After header that the retry below needs.
			RoundTripper: http.DefaultTransport,
		},
	}
}

// maxRetries bounds how many times a rate-limited request is retried before
// giving up. Without this a 429 drops the book entirely, which during a
// library import means silently missing entries rather than a slow one.
const maxRetries = 4

// exhaustedBackoff is the minimum wait reported to the client once our own
// retries are used up. audnexus's window is around a minute in practice, and
// anything shorter just has the client poking a service that's asked it to
// stop.
const exhaustedBackoff = 60 * time.Second

// maxBackoff caps how long a single attempt will wait. An upstream asking for
// several minutes is better handled by failing this book and letting the
// client retry than by pinning a goroutine.
const maxBackoff = 30 * time.Second

func (c *AudibleClient) get(ctx context.Context, client *http.Client, url string, out any) error {
	for attempt := 0; ; attempt++ {
		body, err := c.attempt(ctx, client, url, attempt)

		var retry retryableErr
		if errors.As(err, &retry) {
			if attempt < maxRetries {
				Log(ctx).Debug("backing off", "url", url, "wait", retry.after, "attempt", attempt)
				select {
				case <-time.After(retry.after):
					continue
				case <-ctx.Done():
					return ctx.Err()
				}
			}

			// Out of retries. Report a wait long enough to actually clear the
			// upstream's window: the client retries 429s in an unbounded loop
			// and defaults to five seconds, which keeps a rate-limited
			// upstream rate limited.
			return retryableErr{after: max(retry.after, exhaustedBackoff), err: retry.err}
		}
		if err != nil {
			return err
		}

		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("decoding %s: %w", url, err)
		}
		return nil
	}
}

// retryableErr marks a response worth retrying, and how long to wait first.
type retryableErr struct {
	after time.Duration
	err   error
}

func (e retryableErr) Error() string { return e.err.Error() }
func (e retryableErr) Unwrap() error { return e.err }

// RetryAfter implements retryAfterErr so the handler can pass the wait on to
// the client instead of letting it guess.
func (e retryableErr) RetryAfter() time.Duration { return e.after }

func (c *AudibleClient) attempt(ctx context.Context, client *http.Client, url string, attempt int) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("requesting %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		// audnexus returns 404 both for unknown ASINs and for ASINs that exist
		// in another marketplace. Neither is retryable for us.
		return nil, errors.Join(errNotFound, fmt.Errorf("not found: %s", url))

	case resp.StatusCode == http.StatusTooManyRequests,
		resp.StatusCode == http.StatusServiceUnavailable:
		return nil, retryableErr{
			after: backoff(resp, attempt),
			err: errors.Join(statusErr(resp.StatusCode),
				fmt.Errorf("rate limited by %s", url)),
		}

	case resp.StatusCode >= 300:
		return nil, fmt.Errorf("unexpected status %d from %s", resp.StatusCode, url)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	return body, nil
}

// backoff honors Retry-After when the upstream sends one, and otherwise waits
// a second with jitter so that concurrent refreshes don't retry in lockstep.
func backoff(resp *http.Response, attempt int) time.Duration {
	if v := resp.Header.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			return min(time.Duration(secs)*time.Second, maxBackoff)
		}
	}

	wait := time.Duration(1<<attempt) * time.Second
	wait += time.Duration(rand.Int64N(int64(500 * time.Millisecond)))
	return min(wait, maxBackoff)
}

// GetBook fetches a single book from audnexus.
func (c *AudibleClient) GetBook(ctx context.Context, asin string) (*audnexusBook, error) {
	var book audnexusBook
	u := fmt.Sprintf("https://%s/books/%s?region=%s", c.audnexusHost, url.PathEscape(asin), c.region)
	if err := c.get(ctx, c.audnexus, u, &book); err != nil {
		return nil, err
	}
	if book.ASIN == "" {
		return nil, errors.Join(errNotFound, fmt.Errorf("empty book for %s", asin))
	}
	return &book, nil
}

// GetAuthor fetches a single author from audnexus.
func (c *AudibleClient) GetAuthor(ctx context.Context, asin string) (*audnexusAuthor, error) {
	var author audnexusAuthor
	u := fmt.Sprintf("https://%s/authors/%s?region=%s", c.audnexusHost, url.PathEscape(asin), c.region)
	if err := c.get(ctx, c.audnexus, u, &author); err != nil {
		return nil, err
	}
	if author.ASIN == "" {
		return nil, errors.Join(errNotFound, fmt.Errorf("empty author for %s", asin))
	}
	return &author, nil
}

// SearchProducts runs a natural language query against Audible's catalog.
func (c *AudibleClient) SearchProducts(ctx context.Context, query string, limit int) ([]audibleProduct, error) {
	params := url.Values{}
	params.Set("keywords", query)
	params.Set("num_results", strconv.Itoa(limit))
	params.Set("products_sort_by", "Relevance")
	params.Set("response_groups", productResponseGroups)

	var products audibleProducts
	u := fmt.Sprintf("https://%s/1.0/catalog/products?%s", c.audibleHost, params.Encode())
	if err := c.get(ctx, c.audible, u, &products); err != nil {
		return nil, err
	}
	return products.Products, nil
}

// ProductsByAuthor pages through an author's catalog.
//
// Audible only filters by author *name*, not ASIN, so results are filtered
// against the expected ASIN by the caller — the response includes author ASINs,
// which makes that exact rather than a name-match heuristic.
func (c *AudibleClient) ProductsByAuthor(ctx context.Context, name string, page int) ([]audibleProduct, int, error) {
	params := url.Values{}
	params.Set("author", name)
	params.Set("num_results", strconv.Itoa(audiblePageSize))
	params.Set("page", strconv.Itoa(page))
	params.Set("response_groups", productResponseGroups)

	var products audibleProducts
	u := fmt.Sprintf("https://%s/1.0/catalog/products?%s", c.audibleHost, params.Encode())
	if err := c.get(ctx, c.audible, u, &products); err != nil {
		return nil, 0, err
	}
	return products.Products, products.TotalResults, nil
}

// audiblePageSize is the largest page Audible's catalog will return.
const audiblePageSize = 50

// productResponseGroups is the minimum set of fields we need mapped.
const productResponseGroups = "contributors,media,product_desc,product_attrs,series,rating"

// asBook converts a catalog product into the same shape audnexus returns, so
// one mapping path serves both.
//
// A catalog listing already carries almost everything a work needs, which
// makes walking an author's bibliography nearly free -- otherwise every book
// in it costs its own audnexus request. What's lost is the full description
// (only a truncated merchandising summary is present), genres, and the ISBN.
// Of those only the ISBN is scored during identification, and audiobooks are
// matched on ASIN, which is present.
func (p audibleProduct) asBook() *audnexusBook {
	book := &audnexusBook{
		ASIN:             p.ASIN,
		Title:            p.Title,
		Subtitle:         p.Subtitle,
		Authors:          p.Authors,
		Narrators:        p.Narrators,
		Description:      p.MerchandisingSummary,
		Language:         p.Language,
		PublisherName:    p.PublisherName,
		ReleaseDate:      p.ReleaseDate,
		RuntimeLengthMin: p.RuntimeLengthMin,
		Image:            p.imageURL(),
	}

	if len(p.Series) > 0 {
		book.SeriesPrimary = &audnexusSeriesRef{
			ASIN: p.Series[0].ASIN, Name: p.Series[0].Title, Position: p.Series[0].Sequence,
		}
	}
	if len(p.Series) > 1 {
		book.SeriesSecondary = &audnexusSeriesRef{
			ASIN: p.Series[1].ASIN, Name: p.Series[1].Title, Position: p.Series[1].Sequence,
		}
	}

	return book
}

// imageURL picks the largest cover Audible offers for a product.
func (p audibleProduct) imageURL() string {
	best, bestSize := "", -1
	for size, u := range p.ProductImages {
		n, err := strconv.Atoi(size)
		if err != nil {
			continue
		}
		if n > bestSize {
			best, bestSize = u, n
		}
	}
	return best
}

// creditsAuthor reports whether the product is credited to the given author,
// by mapping key or by name.
//
// Name matching is not redundant: Audible lists the same person under several
// ASINs and omits it entirely on many titles, so an ASIN-only check drops
// books that plainly belong to the author -- the first two Hitchhiker's Guide
// novels among them. Audible's own author filter matches on name, so this
// agrees with the listing that produced these products.
func (p audibleProduct) creditsAuthor(key, name string) bool {
	for _, a := range p.Authors {
		if key != "" && strings.EqualFold(authorKey(a), key) {
			return true
		}
		if name != "" && strings.EqualFold(strings.TrimSpace(a.Name), name) {
			return true
		}
	}
	return false
}
