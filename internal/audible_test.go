package internal

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestAudibleGetter(t *testing.T) *ABGetter {
	t.Helper()

	ids, cache := testDeps(t)

	g, err := NewAudibleGetter(cache, NewAudibleClient("api.audnex.us", "api.audible.com", "us", 0), ids)
	require.NoError(t, err)

	// Seed author detail so mapping doesn't reach the network. A nil entry is
	// a cached miss, which is what an author audnexus doesn't know looks like.
	g.authors["B001IGFHW6"] = &audnexusAuthor{
		ASIN:        "B001IGFHW6",
		Name:        "Brandon Sanderson",
		Description: "Epic fantasy author.",
		Image:       "https://m.media-amazon.com/images/I/author.jpg",
	}
	g.authors["B0UNKNOWN1"] = nil

	return g
}

// testBook mirrors a real audnexus response, trimmed to the fields we map.
func testBook(asin string) *audnexusBook {
	return &audnexusBook{
		ASIN:     asin,
		Title:    "The Final Empire",
		Subtitle: "Mistborn Book 1",
		Authors: []audnexusPerson{
			{ASIN: "B001IGFHW6", Name: "Brandon Sanderson"},
		},
		Narrators:        []audnexusPerson{{Name: "Michael Kramer"}},
		Description:      "For a thousand years the ash fell.",
		Genres:           []audnexusGenre{{Name: "Science Fiction & Fantasy", Type: "genre"}},
		Image:            "https://m.media-amazon.com/images/I/917NNRCArfL.jpg",
		ISBN:             "9781427206374",
		Language:         "english",
		PublisherName:    "Macmillan Audio",
		Rating:           "4.8",
		Region:           "us",
		ReleaseDate:      "2008-12-28T00:00:00.000Z",
		RuntimeLengthMin: 1479,
		SeriesPrimary:    &audnexusSeriesRef{ASIN: "B006K1P698", Name: "The Mistborn Saga", Position: "1"},
	}
}

// TestAudibleBookDataIntegrity mirrors TestGetBookDataIntegrity for the
// Audible upstream. The client is particularly sensitive to null values. For a
// given work resource it MUST
//   - have non-null top-level books
//   - non-null ratingcount, averagerating
//   - have a contributor with a foreign id
func TestAudibleBookDataIntegrity(t *testing.T) {
	g := newTestAudibleGetter(t)
	ctx := t.Context()

	work, err := g.mapBook(ctx, testBook(testASIN(t)))
	require.NoError(t, err)

	require.Len(t, work.Books, 1)
	require.Len(t, work.Authors, 1)
	require.Len(t, work.Authors[0].Works, 1)

	book := work.Books[0]
	require.Len(t, book.Contributors, 1)
	assert.NotZero(t, book.Contributors[0].ForeignID)
	assert.Equal(t, "Author", book.Contributors[0].Role)
	assert.Equal(t, work.Authors[0].ForeignID, book.Contributors[0].ForeignID)

	assert.NotZero(t, work.ForeignID)
	assert.NotZero(t, book.ForeignID)
	assert.Equal(t, book.ForeignID, work.BestBookID)

	// Work and edition describe the same product but must not share an ID.
	assert.NotEqual(t, work.ForeignID, book.ForeignID)

	// Serialized output must not contain nulls in the collections the client
	// walks unconditionally.
	out, err := json.Marshal(work)
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(out, &raw))

	for _, field := range []string{"Books", "Authors", "Series", "Genres", "RelatedWorks"} {
		assert.NotNil(t, raw[field], "%s must not be null", field)
	}
}

// TestAudibleEmbeddedAuthor covers the author embedded in a work. The client
// renders this in search results rather than what GetAuthor returns, so a stub
// here makes every author show up blank.
func TestAudibleEmbeddedAuthor(t *testing.T) {
	g := newTestAudibleGetter(t)
	ctx := t.Context()

	work, err := g.mapBook(ctx, testBook(testASIN(t)))
	require.NoError(t, err)

	author := work.Authors[0]
	assert.Equal(t, "Brandon Sanderson", author.Name)
	assert.Equal(t, "Epic fantasy author.", author.Description)
	assert.Equal(t, "https://m.media-amazon.com/images/I/author.jpg", author.ImageURL)
}

// TestAudibleEmbeddedAuthorUnknown covers an author audnexus has no record of.
// The work still has to be usable, and Description must not be empty.
func TestAudibleEmbeddedAuthorUnknown(t *testing.T) {
	g := newTestAudibleGetter(t)
	ctx := t.Context()

	book := testBook(testASIN(t))
	book.Authors = []audnexusPerson{{ASIN: "B0UNKNOWN1", Name: "Obscure Author"}}

	work, err := g.mapBook(ctx, book)
	require.NoError(t, err)

	author := work.Authors[0]
	assert.Equal(t, "Obscure Author", author.Name)
	assert.Equal(t, "N/A", author.Description)
	assert.Empty(t, author.ImageURL)
}

func TestAudibleBookMapping(t *testing.T) {
	g := newTestAudibleGetter(t)
	ctx := t.Context()

	work, err := g.mapBook(ctx, testBook(testASIN(t)))
	require.NoError(t, err)

	book := work.Books[0]

	t.Run("splits title and subtitle", func(t *testing.T) {
		assert.Equal(t, "The Final Empire", work.Title)
		assert.Equal(t, "The Final Empire", work.ShortTitle)
		assert.Equal(t, "The Final Empire: Mistborn Book 1", work.FullTitle)
	})

	t.Run("marks the edition as audio", func(t *testing.T) {
		assert.False(t, book.IsEbook)
		assert.Equal(t, "Audiobook", book.Format)
	})

	t.Run("carries identifiers through", func(t *testing.T) {
		// The ASIN is what makes matching against existing audio files work,
		// so it has to survive the mapping.
		assert.NotEmpty(t, book.Asin)
		assert.Equal(t, "9781427206374", book.Isbn13)
	})

	t.Run("surfaces the narrator", func(t *testing.T) {
		// There's no narrator field in the schema, so it rides on edition info.
		assert.Equal(t, "Narrated by Michael Kramer", book.EditionInformation)
	})

	t.Run("normalizes release date", func(t *testing.T) {
		assert.Equal(t, "2008-12-28", book.ReleaseDate)
		assert.Equal(t, "2008-12-28T00:00:00.000Z", book.ReleaseDateRaw)
		assert.Equal(t, "2008-12-28", work.ReleaseDate)
	})

	t.Run("normalizes language", func(t *testing.T) {
		assert.Equal(t, "eng", book.Language)
	})

	t.Run("parses the rating", func(t *testing.T) {
		assert.InDelta(t, 4.8, book.AverageRating, 0.001)
	})
}

func TestAudibleBookWithoutAuthor(t *testing.T) {
	g := newTestAudibleGetter(t)
	ctx := t.Context()

	// A work with no identifiable author at all is unusable to the client, so
	// this must fail rather than produce one. A credit with only a name is
	// still an author -- see TestBookWithoutAuthorASIN.
	book := testBook(testASIN(t))
	book.Authors = []audnexusPerson{{}}

	_, err := g.mapBook(ctx, book)
	require.Error(t, err)
	assert.ErrorIs(t, err, errNotFound)
}

func TestAudibleBookMissingFields(t *testing.T) {
	g := newTestAudibleGetter(t)
	ctx := t.Context()

	book := testBook(testASIN(t))
	book.Description = ""
	book.Summary = ""
	book.Genres = nil
	book.Subtitle = ""
	book.SeriesPrimary = nil
	book.Narrators = nil
	book.Rating = ""
	book.ReleaseDate = ""

	work, err := g.mapBook(ctx, book)
	require.NoError(t, err)

	// Description and genres must be set to something; the client rejects
	// empty values here.
	assert.Equal(t, "N/A", work.Books[0].Description)
	assert.Equal(t, []string{"none"}, work.Genres)

	assert.Equal(t, work.Title, work.FullTitle)
	assert.Empty(t, work.Series)
	assert.Empty(t, work.Books[0].EditionInformation)
	assert.Zero(t, work.Books[0].AverageRating)
	assert.Empty(t, work.Books[0].ReleaseDate)
}

func TestAudibleSeriesMapping(t *testing.T) {
	g := newTestAudibleGetter(t)
	ctx := t.Context()

	book := testBook(testASIN(t))
	book.SeriesSecondary = &audnexusSeriesRef{ASIN: "B0DMXTJ8WH", Name: "The Cosmere", Position: ""}

	work, err := g.mapBook(ctx, book)
	require.NoError(t, err)

	require.Len(t, work.Series, 2)

	primary := work.Series[0]
	assert.Equal(t, "The Mistborn Saga", primary.Title)
	assert.NotZero(t, primary.ForeignID)
	require.Len(t, primary.LinkItems, 1)
	assert.Equal(t, 1, primary.LinkItems[0].SeriesPosition)
	assert.Equal(t, "1", primary.LinkItems[0].PositionInSeries)
	assert.Equal(t, work.ForeignID, primary.LinkItems[0].ForeignWorkID)
	assert.True(t, primary.LinkItems[0].Primary)

	secondary := work.Series[1]
	assert.Equal(t, "The Cosmere", secondary.Title)
	assert.NotEqual(t, primary.ForeignID, secondary.ForeignID)
	assert.False(t, secondary.LinkItems[0].Primary)
	// An unnumbered entry shouldn't be forced to position 0 in the raw field.
	assert.Empty(t, secondary.LinkItems[0].PositionInSeries)
}

// TestAudibleSeriesTitleRecorded covers the reason labels exist: Audible has
// no endpoint to resolve a series ASIN back to a name, so GetSeries can only
// work if the title was recorded when the book was first mapped.
func TestAudibleSeriesTitleRecorded(t *testing.T) {
	g := newTestAudibleGetter(t)
	ctx := t.Context()

	book := testBook(testASIN(t))
	book.SeriesPrimary.ASIN = testASIN(t)

	work, err := g.mapBook(ctx, book)
	require.NoError(t, err)
	require.Len(t, work.Series, 1)

	ref, err := g.ids.Ref(ctx, work.Series[0].ForeignID)
	require.NoError(t, err)
	assert.Equal(t, kindSeries, ref.kind)
	assert.Equal(t, "The Mistborn Saga", ref.label)
}

func TestAudibleReleaseDate(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name    string
		in      string
		want    string
		wantRaw string
	}{
		{"audnexus timestamp", "2008-12-28T00:00:00.000Z", "2008-12-28", "2008-12-28T00:00:00.000Z"},
		{"rfc3339", "2008-12-28T08:01:00Z", "2008-12-28", "2008-12-28T08:01:00Z"},
		{"date only", "2008-12-28", "2008-12-28", "2008-12-28"},
		{"empty", "", "", ""},
		{"unparseable", "sometime", "", "sometime"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, raw := audibleReleaseDate(tt.in)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.wantRaw, raw)
		})
	}
}

func TestNormalizeLanguage(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]string{
		"english":  "eng",
		"English":  "eng",
		" german ": "ger",
		"japanese": "jpn",
		"":         "",
		"klingon":  "klingon", // Unknown values pass through rather than vanish.
	} {
		assert.Equal(t, want, normalizeLanguage(in), "input %q", in)
	}
}

func TestAudibleProductHelpers(t *testing.T) {
	t.Parallel()

	p := audibleProduct{
		Authors: []audnexusPerson{{ASIN: "B001IGFHW6", Name: "Brandon Sanderson"}},
		ProductImages: map[string]string{
			"500":  "https://example.invalid/500.jpg",
			"1024": "https://example.invalid/1024.jpg",
			"bad":  "https://example.invalid/bad.jpg",
		},
	}

	t.Run("picks the largest cover", func(t *testing.T) {
		assert.Equal(t, "https://example.invalid/1024.jpg", p.imageURL())
	})

	t.Run("matches authors by key or name", func(t *testing.T) {
		assert.True(t, p.creditsAuthor("B001IGFHW6", ""))
		assert.True(t, p.creditsAuthor("b001igfhw6", ""))
		assert.False(t, p.creditsAuthor("B0719B6Z8Y", ""))

		// Audible omits the ASIN on many titles and lists one person under
		// several, so a name match has to count too.
		assert.True(t, p.creditsAuthor("", "Brandon Sanderson"))
		assert.False(t, p.creditsAuthor("", "Someone Else"))
	})

	t.Run("handles missing images", func(t *testing.T) {
		assert.Empty(t, audibleProduct{}.imageURL())
	})
}

func TestAuthorASIN(t *testing.T) {
	t.Parallel()

	assert.Empty(t, authorASIN(nil))
	assert.Empty(t, authorASIN([]audnexusPerson{{}}))

	// A named credit with no ASIN keys on the name rather than being dropped.
	assert.Equal(t, "name:no asin", authorASIN([]audnexusPerson{{Name: "No ASIN"}}))

	// Co-authors are common and the first credited ASIN wins.
	assert.Equal(t, "B002", authorASIN([]audnexusPerson{
		{Name: "Contributor"},
		{ASIN: "B002", Name: "Author"},
		{ASIN: "B003", Name: "Co-author"},
	}))
}

func TestPersonNames(t *testing.T) {
	t.Parallel()

	assert.Empty(t, personNames(nil))
	assert.Equal(t, []string{"A", "B"}, personNames([]audnexusPerson{
		{Name: "A"}, {Name: ""}, {Name: "B"},
	}))
}

// TestRetryAfterPropagated covers the header the client needs to back off
// sensibly. Without it the client retries a 429 every five seconds in an
// unbounded loop, which keeps the upstream rate limited and stalls an import
// on a single book.
func TestRetryAfterPropagated(t *testing.T) {
	t.Parallel()

	h := &Handler{}

	t.Run("sets the header for a retryable error", func(t *testing.T) {
		t.Parallel()

		w := httptest.NewRecorder()
		h.error(w, retryableErr{
			after: 90 * time.Second,
			err:   errors.Join(statusErr(http.StatusTooManyRequests), errors.New("rate limited")),
		})

		assert.Equal(t, http.StatusTooManyRequests, w.Code)
		assert.Equal(t, "90", w.Header().Get("Retry-After"))
	})

	t.Run("leaves it off for other errors", func(t *testing.T) {
		t.Parallel()

		w := httptest.NewRecorder()
		h.error(w, errNotFound)

		assert.Equal(t, http.StatusNotFound, w.Code)
		assert.Empty(t, w.Header().Get("Retry-After"))
	})
}

// TestProductAsBook covers mapping an author-walk book from the catalog
// listing instead of a per-book audnexus request. Everything identification
// scores on -- author, title, series, year, ASIN -- has to survive; only the
// ISBN and the full description are knowingly given up.
func TestProductAsBook(t *testing.T) {
	t.Parallel()

	p := audibleProduct{
		ASIN:                 "B002V0QCYU",
		Title:                "The Final Empire",
		Subtitle:             "Mistborn Book 1",
		Authors:              []audnexusPerson{{ASIN: "B001IGFHW6", Name: "Brandon Sanderson"}},
		Narrators:            []audnexusPerson{{Name: "Michael Kramer"}},
		MerchandisingSummary: "For a thousand years the ash fell.",
		PublisherName:        "Macmillan Audio",
		ReleaseDate:          "2008-12-28",
		Language:             "english",
		RuntimeLengthMin:     1479,
		ProductImages:        map[string]string{"500": "https://example.invalid/500.jpg"},
		Series: []audibleSeries{
			{ASIN: "B006K1P698", Title: "The Mistborn Saga", Sequence: "1"},
			{ASIN: "B0DMXTJ8WH", Title: "The Cosmere", Sequence: ""},
		},
	}

	book := p.asBook()

	assert.Equal(t, "B002V0QCYU", book.ASIN)
	assert.Equal(t, "The Final Empire", book.Title)
	assert.Equal(t, "Mistborn Book 1", book.Subtitle)
	assert.Equal(t, "2008-12-28", book.ReleaseDate)
	assert.Equal(t, "https://example.invalid/500.jpg", book.Image)

	require.Len(t, book.Authors, 1)
	assert.Equal(t, "B001IGFHW6", book.Authors[0].ASIN)
	require.Len(t, book.Narrators, 1)

	require.NotNil(t, book.SeriesPrimary)
	assert.Equal(t, "The Mistborn Saga", book.SeriesPrimary.Name)
	assert.Equal(t, "1", book.SeriesPrimary.Position)
	require.NotNil(t, book.SeriesSecondary)
	assert.Equal(t, "The Cosmere", book.SeriesSecondary.Name)

	// Knowingly absent -- catalog listings carry neither.
	assert.Empty(t, book.ISBN)
	assert.Empty(t, book.Genres)
}

// TestWorkResourcePrefersProduct checks that a cached catalog entry is used
// instead of an audnexus request. The test client points at a host that
// doesn't resolve, so a lookup would fail rather than silently pass.
func TestWorkResourcePrefersProduct(t *testing.T) {
	ids, cache := testDeps(t)
	ctx := t.Context()

	g, err := NewAudibleGetter(cache, NewAudibleClient("invalid.invalid", "invalid.invalid", "us", 0), ids)
	require.NoError(t, err)

	asin := testASIN(t)
	g.authors["B001IGFHW6"] = &audnexusAuthor{ASIN: "B001IGFHW6", Name: "Brandon Sanderson"}
	g.rememberProducts([]audibleProduct{{
		ASIN:    asin,
		Title:   "The Final Empire",
		Authors: []audnexusPerson{{ASIN: "B001IGFHW6", Name: "Brandon Sanderson"}},
	}})

	work, err := g.workResource(ctx, asin)
	require.NoError(t, err)
	assert.Equal(t, "The Final Empire", work.Title)
	require.Len(t, work.Books, 1)
	assert.Equal(t, asin, work.Books[0].Asin)
}

// TestAuthorIdentityNotMixed covers a co-authored book whose first credit has
// no ASIN. The name and the ASIN must describe the same person: taking the
// name from one entry and the ID from another files one author's books under
// another author's name.
func TestAuthorIdentityNotMixed(t *testing.T) {
	g := newTestAudibleGetter(t)
	ctx := t.Context()

	g.authors["B0COAUTHOR"] = &audnexusAuthor{ASIN: "B0COAUTHOR", Name: "Real Author"}

	book := testBook(testASIN(t))
	book.Authors = []audnexusPerson{
		{Name: "Uncredited Contributor"}, // no ASIN
		{ASIN: "B0COAUTHOR", Name: "Real Author"},
	}

	work, err := g.mapBook(ctx, book)
	require.NoError(t, err)

	author := work.Authors[0]
	assert.Equal(t, "Real Author", author.Name, "name must match the ASIN it was minted from")

	// The surrogate ID must belong to the same person as the name.
	expected, err := g.ids.ID(ctx, kindAuthor, "B0COAUTHOR", "")
	require.NoError(t, err)
	assert.Equal(t, expected, author.ForeignID)

	// And the recorded label must not be the other contributor's name.
	ref, err := g.ids.Ref(ctx, author.ForeignID)
	require.NoError(t, err)
	assert.Equal(t, "B0COAUTHOR", ref.asin)
	assert.NotEqual(t, "Uncredited Contributor", ref.label)
}

func TestPrimaryAuthor(t *testing.T) {
	t.Parallel()

	_, ok := primaryAuthor(nil)
	assert.False(t, ok)

	// A name with no ASIN is still an author; only an empty credit isn't.
	a0, ok := primaryAuthor([]audnexusPerson{{Name: "No ASIN"}})
	require.True(t, ok)
	assert.Equal(t, "No ASIN", a0.Name)

	_, ok = primaryAuthor([]audnexusPerson{{}})
	assert.False(t, ok)

	a, ok := primaryAuthor([]audnexusPerson{
		{Name: "Contributor"},
		{ASIN: "B002", Name: "Author"},
		{ASIN: "B003", Name: "Co-author"},
	})
	require.True(t, ok)
	assert.Equal(t, "B002", a.ASIN)
	assert.Equal(t, "Author", a.Name, "name and ASIN must come from one entry")
}

// TestPrimaryAuthorSkipsContributors covers Audible's role suffixes. An
// anthology often credits its editor first, and treating that as the author
// files the book under the wrong person and gives the editor a bibliography of
// their own for the controller to walk.
func TestPrimaryAuthorSkipsContributors(t *testing.T) {
	t.Parallel()

	a, ok := primaryAuthor([]audnexusPerson{
		{ASIN: "B0EDITOR001", Name: "Shawn Speakman - editor"},
		{ASIN: "B0TRANS0001", Name: "Constance Garnett - translator"},
		{ASIN: "B0AUTHOR001", Name: "Ursula K. Le Guin"},
	})
	require.True(t, ok)
	assert.Equal(t, "Ursula K. Le Guin", a.Name)

	// Credited only to an editor: the book still needs an author.
	a, ok = primaryAuthor([]audnexusPerson{
		{ASIN: "B0EDITOR001", Name: "Shawn Speakman - editor"},
	})
	require.True(t, ok)
	assert.Equal(t, "B0EDITOR001", a.ASIN)

	for _, name := range []string{
		"Someone - editor", "Someone - translator", "Someone - adaptation",
		"Someone - introduction", "Someone - contributor", "Someone - foreword",
	} {
		assert.True(t, _contributorRole.MatchString(name), name)
	}
	assert.False(t, _contributorRole.MatchString("Ursula K. Le Guin"))
	assert.False(t, _contributorRole.MatchString("Editor Jones"))
}

// TestAuthorKeyFallsBackToName covers Audible titles credited to a named
// author with no ASIN -- the first two Hitchhiker's Guide novels among them.
// Requiring an ASIN dropped those books from the library entirely.
func TestAuthorKeyFallsBackToName(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "B000AQ2A84", authorKey(audnexusPerson{ASIN: "B000AQ2A84", Name: "Douglas Adams"}))
	assert.Equal(t, "name:douglas adams", authorKey(audnexusPerson{Name: "Douglas Adams"}))
	assert.Equal(t, "name:douglas adams", authorKey(audnexusPerson{Name: " Douglas ADAMS "}))
	assert.Empty(t, authorKey(audnexusPerson{}))

	name, ok := authorName("name:douglas adams")
	assert.True(t, ok)
	assert.Equal(t, "douglas adams", name)

	_, ok = authorName("B000AQ2A84")
	assert.False(t, ok)

	// An author page only exists for a real ASIN.
	assert.Empty(t, authorURL("name:douglas adams"))
	assert.NotEmpty(t, authorURL("B000AQ2A84"))
}

// TestBookWithoutAuthorASIN is the Hitchhiker's Guide case: a real author, no
// ASIN. The book must still map rather than being dropped.
func TestBookWithoutAuthorASIN(t *testing.T) {
	g := newTestAudibleGetter(t)
	ctx := t.Context()

	book := testBook(testASIN(t))
	book.Authors = []audnexusPerson{{Name: "Douglas Adams"}}

	work, err := g.mapBook(ctx, book)
	require.NoError(t, err)

	require.Len(t, work.Authors, 1)
	assert.Equal(t, "Douglas Adams", work.Authors[0].Name)
	assert.NotZero(t, work.Authors[0].ForeignID)

	ref, err := g.ids.Ref(ctx, work.Authors[0].ForeignID)
	require.NoError(t, err)
	assert.Equal(t, "name:douglas adams", ref.asin)
}
