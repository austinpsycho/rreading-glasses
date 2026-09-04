package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// NewAudibleClient returns a client for the given hosts and region.
func NewAudibleClient(audnexusHost, audibleHost, region string) *AudibleClient {
	if region == "" {
		region = defaultRegion
	}
	return &AudibleClient{
		audnexus:     newThrottledClient(),
		audible:      newThrottledClient(),
		audnexusHost: audnexusHost,
		audibleHost:  audibleHost,
		region:       region,
	}
}

// newThrottledClient rate limits an unauthenticated upstream. Neither API
// documents a limit, and both will start refusing traffic if pushed, so this
// is deliberately conservative.
func newThrottledClient() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: throttledTransport{
			ticker:       time.NewTicker(time.Second / 3),
			RoundTripper: errorProxyTransport{http.DefaultTransport},
		},
	}
}

func (c *AudibleClient) get(ctx context.Context, client *http.Client, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("requesting %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		// audnexus returns 404 both for unknown ASINs and for ASINs that exist
		// in another marketplace. Neither is retryable for us.
		return errors.Join(errNotFound, fmt.Errorf("not found: %s", url))
	case resp.StatusCode == http.StatusTooManyRequests:
		return fmt.Errorf("rate limited by %s", url)
	case resp.StatusCode >= 300:
		return fmt.Errorf("unexpected status %d from %s", resp.StatusCode, url)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}

	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decoding %s: %w", url, err)
	}

	return nil
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

// hasAuthor reports whether the product is credited to the given author ASIN.
func (p audibleProduct) hasAuthor(asin string) bool {
	for _, a := range p.Authors {
		if strings.EqualFold(a.ASIN, asin) {
			return true
		}
	}
	return false
}
