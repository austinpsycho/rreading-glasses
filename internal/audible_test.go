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

	// Seed author detail so mapping doesn't reach the network. Detail is keyed
	// by author key, not ASIN. A nil entry is a cached miss, which is what an
	// author audnexus has no record of looks like.
	g.authors["name:brandon sanderson"] = &audnexusAuthor{
		ASIN:        "B001IGFHW6",
		Name:        "Brandon Sanderson",
		Description: "Epic fantasy author.",
		Image:       "https://m.media-amazon.com/images/I/author.jpg",
	}
	g.authors["name:obscure author"] = nil
	g.authors["name:henry james"] = nil
	g.authors["name:david ludwig"] = nil

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
	book.Authors = []audnexusPerson{{Name: "Obscure Author"}}

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
		assert.True(t, p.creditsAuthor("name:brandon sanderson", ""))
		assert.False(t, p.creditsAuthor("name:someone else", ""))

		// The walk also matches by name, since Audible varies how it credits
		// the same person between titles.
		assert.True(t, p.creditsAuthor("", "Brandon Sanderson"))
		assert.False(t, p.creditsAuthor("", "Someone Else"))
	})

	t.Run("handles missing images", func(t *testing.T) {
		assert.Empty(t, audibleProduct{}.imageURL())
	})
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

// TestAuthorURLNeverEmpty guards the client's add-author view, which reads
// links[0].url without checking. The client only records a link when the URL
// is non-empty, so an author with none crashes the page.
func TestAuthorURLNeverEmpty(t *testing.T) {
	t.Parallel()

	assert.NotEmpty(t, authorURL("B000AQ2A84"))
	assert.NotEmpty(t, authorURL("name:douglas adams"))
	assert.Contains(t, authorURL("name:douglas adams"), "douglas+adams")
}

// TestAuthorKeyIsTheName pins author identity to the normalized name. Audible
// omits the ASIN on many titles, lists one person under several, and varies
// the name with credentials, so anything keyed on ASINs either drops books or
// splits an author across entries.
func TestAuthorKeyIsTheName(t *testing.T) {
	t.Parallel()

	// The ASIN is deliberately ignored.
	assert.Equal(t, "name:douglas adams",
		authorKey(audnexusPerson{ASIN: "B000AQ2A84", Name: "Douglas Adams"}))
	assert.Equal(t, "name:douglas adams", authorKey(audnexusPerson{Name: "Douglas Adams"}))
	assert.Equal(t, "name:douglas adams", authorKey(audnexusPerson{Name: " Douglas  ADAMS "}))
	assert.Empty(t, authorKey(audnexusPerson{}))

	// Credentials vary between titles by one author and must not split them.
	for _, name := range []string{
		"David Ludwig", "David Ludwig MD", "David Ludwig MD PhD",
		"David Ludwig MD PhD MD PhD", "David Ludwig, PhD", "David Ludwig - editor",
	} {
		assert.Equal(t, "name:david ludwig", authorKey(audnexusPerson{Name: name}), name)
	}

	name, ok := authorName("name:douglas adams")
	assert.True(t, ok)
	assert.Equal(t, "douglas adams", name)

	assert.NotEmpty(t, authorURL("name:douglas adams"))
}

// TestAuthorKeyIsOrderIndependent is the property the alias table could not
// provide: a credit resolves the same way regardless of what was seen first,
// so an author can't be split by fetch order.
func TestAuthorKeyIsOrderIndependent(t *testing.T) {
	g := newTestAudibleGetter(t)
	ctx := t.Context()

	credits := [][]audnexusPerson{
		{{ASIN: "B000APYNL2", Name: "Henry James"}},
		{{Name: "Henry James"}},
		{{Name: "Henry James MD"}},
		{{ASIN: "B0OTHERASIN", Name: "henry james"}},
	}

	var first int64
	for i, credit := range credits {
		book := testBook(testASIN(t))
		book.Authors = credit

		work, err := g.mapBook(ctx, book)
		require.NoError(t, err)

		if i == 0 {
			first = work.Authors[0].ForeignID
			continue
		}
		assert.Equal(t, first, work.Authors[0].ForeignID,
			"credit %d must resolve to the same author", i)
	}
}

// TestSearchAndMapAgreeOnAuthor pins search and mapping to one author ID. When
// they disagreed, GetAuthor found no works for the ID the client had been
// given and reported the author as missing.
func TestSearchAndMapAgreeOnAuthor(t *testing.T) {
	g := newTestAudibleGetter(t)
	ctx := t.Context()

	credits := []audnexusPerson{{Name: "David Ludwig MD PhD"}}

	rsc, err := g.searchResource(ctx, testASIN(t), g.authorKeyOf(credits), authorDisplayName(credits))
	require.NoError(t, err)

	book := testBook(testASIN(t))
	book.Authors = credits

	work, err := g.mapBook(ctx, book)
	require.NoError(t, err)

	assert.Equal(t, rsc.Author.ID, work.Authors[0].ForeignID)
	assert.Equal(t, "David Ludwig", work.Authors[0].Name, "credentials are stripped for display")
}

// TestAuthorDisplayNameKeepsCasing covers the name shown in the client. Keys
// are lower cased for identity, so without recording the name as written every
// author reads "david ludwig".
func TestAuthorDisplayNameKeepsCasing(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "David Ludwig", authorDisplayName([]audnexusPerson{{Name: "David Ludwig MD PhD"}}))
	assert.Equal(t, "Brandon Sanderson",
		authorDisplayName([]audnexusPerson{{ASIN: "B001IGFHW6", Name: "Brandon Sanderson"}}))
	assert.Empty(t, authorDisplayName(nil))

	// Last resort when nothing recorded the original spelling.
	assert.Equal(t, "David Ludwig", titleCaseName("david ludwig"))
}

// TestSearchRecordsAuthorName pins the label search writes, which is what the
// author lookup later reads for display.
func TestSearchRecordsAuthorName(t *testing.T) {
	g := newTestAudibleGetter(t)
	ctx := t.Context()

	credits := []audnexusPerson{{Name: "David Ludwig MD PhD"}}

	rsc, err := g.searchResource(ctx, testASIN(t), g.authorKeyOf(credits), authorDisplayName(credits))
	require.NoError(t, err)

	ref, err := g.ids.Ref(ctx, rsc.Author.ID)
	require.NoError(t, err)
	assert.Equal(t, "name:david ludwig", ref.asin)
	assert.Equal(t, "David Ludwig", ref.label)
}

// TestSearchCachesProducts covers the library-import hot path. The client
// follows a search with /book/bulk over every result, so without the listing
// cached each result costs its own audnexus request -- roughly 26 per
// unidentified file rather than one.
func TestSearchCachesProducts(t *testing.T) {
	g := newTestAudibleGetter(t)

	asin := testASIN(t)
	assert.Nil(t, g.product(asin))

	g.rememberProducts([]audibleProduct{{
		ASIN:    asin,
		Title:   "The Final Empire",
		Authors: []audnexusPerson{{ASIN: "B001IGFHW6", Name: "Brandon Sanderson"}},
	}})

	require.NotNil(t, g.product(asin))
	assert.Equal(t, "The Final Empire", g.product(asin).Title)
}

// TestDirectLookupBypassesCatalogCache covers the other half: a bulk load or
// background refresh is served from the listing, but opening one book gets the
// fuller record. The test client points at a host that doesn't resolve, so a
// direct lookup fails rather than silently taking the cheap path.
func TestDirectLookupBypassesCatalogCache(t *testing.T) {
	ids, cache := testDeps(t)

	g, err := NewAudibleGetter(cache, NewAudibleClient("invalid.invalid", "invalid.invalid", "us", 0), ids)
	require.NoError(t, err)

	asin := testASIN(t)
	g.rememberProducts([]audibleProduct{{
		ASIN:    asin,
		Title:   "The Final Empire",
		Authors: []audnexusPerson{{Name: "Brandon Sanderson"}},
	}})

	// Bulk and refresh paths use the listing.
	work, err := g.workResource(t.Context(), asin)
	require.NoError(t, err)
	assert.Equal(t, "The Final Empire", work.Title)

	// A direct lookup tries audnexus first. It can't reach it here, so it
	// falls back to the listing rather than failing the request outright.
	work, err = g.workResource(WithDirectLookup(t.Context()), asin)
	require.NoError(t, err)
	assert.Equal(t, "The Final Empire", work.Title, "a failed detail fetch falls back to the listing")
}

// TestAuthorRecordScore covers preferring the fuller of several records for
// one author. Audible lists the same person under several ASINs and audnexus
// keeps a record per ASIN, so an exact name match isn't unique: "George
// Saunders" returns one with a bio and photo and one with neither.
func TestAuthorRecordScore(t *testing.T) {
	t.Parallel()

	full := &audnexusAuthor{Description: "Writes short stories.", Image: "https://example.invalid/a.jpg"}
	bioOnly := &audnexusAuthor{Description: "Writes short stories."}
	imageOnly := &audnexusAuthor{Image: "https://example.invalid/a.jpg"}
	empty := &audnexusAuthor{}

	assert.Greater(t, authorRecordScore(full), authorRecordScore(bioOnly))
	assert.Greater(t, authorRecordScore(bioOnly), authorRecordScore(imageOnly))
	assert.Greater(t, authorRecordScore(imageOnly), authorRecordScore(empty))
	assert.Equal(t, 0, authorRecordScore(empty))
}
