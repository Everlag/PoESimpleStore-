package dbTest

import (
	"testing"
	"time"

	"github.com/Everlag/poeitemstore/cmd"
	"github.com/Everlag/poeitemstore/db"
	"github.com/Everlag/poeitemstore/stash"
	"github.com/boltdb/bolt"
)

// MultiModSearchToIndexQuery converts a MultiModSearch
// into an IndexQuery. It also returns the league because
// you usually need that...
func MultiModSearchToIndexQuery(search cmd.MultiModSearch,
	bdb *bolt.DB, t testing.TB) (db.IndexQuery, db.LeagueHeapID) {

	if len(search.MinValues) != len(search.Mods) {
		t.Fatalf("each mod must have a minvalue")
	}

	// Lookup the root, flavor, and mod
	strings := []string{search.RootType, search.RootFlavor}
	ids, err := db.GetStrings(strings, bdb)
	if err != nil {
		t.Fatalf("failed to fetch rootType or RootFlavor id, err=%s\n", err)
	}
	modIds, err := db.GetStrings(search.Mods, bdb)
	if err != nil {
		t.Fatalf("failed to fetch mod id, err=%s\n", err)
	}

	// And we we need to fetch the league
	leagueIDs, err := db.GetLeagues([]string{search.League}, bdb)
	if err != nil {
		t.Fatalf("failed to fetch league, err=%s\n", err)
	}

	return db.NewIndexQuery(ids[0], ids[1],
		modIds, search.MinValues, leagueIDs[0], search.MaxDesired), leagueIDs[0]

}

// IndexQueryWithResultsToItemStoreQuery converts a MultiModSearch
// into an ItemStoreQuery while attempting to preserve the semantics
// of an IndexQuery in the resulting ItemStoreQuery
//
// IndexQuery has results ordered by highest values
// while ItemStoreQuery has results ordered by latest additions
// with minimum values.
func IndexQueryWithResultsToItemStoreQuery(search cmd.MultiModSearch,
	prevResults []stash.Item,
	bdb *bolt.DB, t testing.TB) db.ItemStoreQuery {

	if len(search.MinValues) != len(search.Mods) {
		t.Fatalf("each mod must have a minvalue")
	}

	// Setup a interestedMap so we can have constant time lookup
	// for which mods we are interesed in
	interestedMap := make(map[string]struct{})
	for _, mod := range search.Mods {
		interestedMap[mod] = struct{}{}
	}

	// Setup the minValue map, this will determine the real minimum
	// values which the ItemStoreQuery will need to find
	minValueMap := make(map[string]uint16)
	for _, item := range prevResults {
		for _, mod := range item.GetMods() {
			// Check if we are about this mod
			_, ok := interestedMap[string(mod.Template)]
			if !ok {
				continue
			}

			// Update the minValues as necessary
			prev, ok := minValueMap[string(mod.Template)]
			if !ok {
				prev = mod.Values[0]
			}
			if prev >= mod.Values[0] {
				minValueMap[string(mod.Template)] = mod.Values[0]
			}
		}
	}
	// Populate any non-present mods with pre-existing values, found items will
	// always be equal to or higher than the pre-existing
	for i, mod := range search.Mods {
		if _, ok := minValueMap[mod]; !ok {
			minValueMap[mod] = search.MinValues[i]
		}
	}

	// Overwrite the search with the new minimum values
	prevLength := len(search.Mods) // Store old length for later
	search.Mods = make([]string, 0)
	search.MinValues = make([]uint16, 0)
	for mod, min := range minValueMap {
		search.Mods = append(search.Mods, mod)
		search.MinValues = append(search.MinValues, min)
	}
	if len(search.Mods) != prevLength {
		t.Fatalf("bad MultiModSearch translation: mismatched #mods")
	}

	t.Logf("Generated MultiModSearch:\n %s", search.String())

	itemStoreSearch, _ := MultiModSearchToItemStoreQuery(search, bdb, t)
	return itemStoreSearch

}

// ChangeSetUse is the callback given to RunChangeSet
// to make traversing a ChangeSet less awful.
//
// ChangeSetUse is expected to be an anonymous function
// accessing the database through its defining scope.
type ChangeSetUse func(id string) error

// RunChangeSet steps through a given ChangeSet, adding changes
// to the provided DB then calling cb to do some work
// on the database.
//
// when + timeDelta * changeIndex will be used as the provided
// time for a Change.
//
// cb will we called for each entry in the ChangeSet
func RunChangeSet(set stash.ChangeSet, cb ChangeSetUse,
	when time.Time, timeDelta time.Duration,
	bdb *bolt.DB, t testing.TB) {

	// Generate a mapping of change to id we'll need
	inverter := GetChangeSetInverter(set)

	for i, comp := range set.Changes {
		// Decompress
		id := inverter[i]
		resp, err := comp.Decompress()
		if err != nil {
			t.Fatalf("failed to decompress stash.Compressed, changeID=%s err=%s",
				id, err)
		}

		// Display status only during tests
		_, ok := t.(*testing.T)
		if ok {
			t.Logf("processing changeID=%s", id)
		}

		cStashes, cItems, err := db.StashStashToCompact(resp.Stashes, TimeOfStart,
			bdb)
		if err != nil {
			t.Fatalf("failed to convert fat stashes to compact, err=%s\n", err)
		}

		_, err = db.AddStashes(cStashes, cItems, bdb)
		if err != nil {
			t.Fatalf("failed to AddStashes, err=%s", err)
		}

		if err := cb(id); err != nil {
			t.Fatalf("failed to cb in RunChangeSet, err=%s", err)
		}

		when = when.Add(timeDelta)
	}

}

var QueryBootsMovespeedFireResist = cmd.MultiModSearch{
	MaxDesired: 4,
	RootType:   "Armour",
	RootFlavor: "Boots",
	League:     "Legacy",
	Mods: []string{
		"#% increased Movement Speed",
		"+#% to Fire Resistance",
	},
	MinValues: []uint16{
		24,
		27,
	},
}

var QueryAmuletColdCritMulti = cmd.MultiModSearch{
	MaxDesired: 4,
	RootType:   "Jewelry",
	RootFlavor: "Amulet",
	League:     "Legacy",
	Mods: []string{
		"#% increased Cold Damage",
		"+#% to Global Critical Strike Multiplier",
	},
	MinValues: []uint16{
		10,
		10,
	},
}

// testIndexQueryAgainstChangeSet ensures a given MultiModSearch
// is valid for every change in the ChangeSet located at path
func testIndexQueryAgainstChangeSet(search cmd.MultiModSearch, path string,
	t *testing.T) {

	t.Parallel()

	bdb := NewTempDatabase(t)

	// Fetch the changes we need
	set := GetChangeSet(path, t)
	if len(set.Changes) != 11 {
		t.Fatalf("wrong number of changes, expected 11 got %d",
			len(set.Changes))
	}

	// We have to find items that match at least once or else the test
	// is absolutely useless.
	foundOnce := false

	RunChangeSet(set, func(id string) error {
		success := t.Run(id, func(t *testing.T) {
			// Translate the query now, after we are more likely
			// to have the desired mods available on the StringHeap
			indexQuery, league := MultiModSearchToIndexQuery(search, bdb, t)

			indexResult, err := indexQuery.Run(bdb)
			if err != nil {
				t.Fatalf("failed IndexQuery.Run, err=%s", err)
			}

			foundOnce = foundOnce || (len(indexResult) > 0)
			if len(indexResult) > 0 {
				t.Logf("found %d items", len(indexResult))
			}

			// Ensure correctness
			CompareIndexQueryResultsToItemStoreEquiv(search, indexResult, league,
				bdb, t)
		})
		if !success {
			t.Fatalf("failed subtest '%s'", id)
		}
		return nil
	}, TimeOfStart, TestTimeDeltas, bdb, t)

	if !foundOnce {
		t.Fatalf("failed to match any items across all queries")
	}
}

// Test as searching across multiple stash updates
func TestIndexQuery11UpdatesMovespeedFireResist(t *testing.T) {
	testIndexQueryAgainstChangeSet(QueryBootsMovespeedFireResist.Clone(),
		"testSet - 11 updates.msgp", t)
}

// Test as searching across multiple stash updates
func TestIndexQuery11UpdatesColdCritMulti(t *testing.T) {
	testIndexQueryAgainstChangeSet(QueryAmuletColdCritMulti.Clone(),
		"testSet - 11 updates.msgp", t)
}

// Test removals to a single stash on a per-item level
//
// This also ensures items are properly removed from the index
func TestIndexRemovalSingleStash(t *testing.T) {

	t.Parallel()

	bdb := NewTempDatabase(t)

	// Define our search up here, it will be constant for all of
	// our sub-tests
	search := QueryRingStrengthIntES.Clone()

	expected := []db.GGGID{
		db.GGGIDFromUID("3d474bb6f4d2b3bf86c0911aac89b5c50bef1d556240f745936df3b7d78a1db1"),
		db.GGGIDFromUID("0125dab1d32f9e28d5531900d0d654774e7d8fc1e26bc717ada8e49231990f61"),
	}

	// Test to ensure we can handle a single update
	t.Run("Baseline", func(t *testing.T) {
		stashes, items := GetTestStashUpdate("singleStash - 3ItemsAdded.json",
			bdb, t)

		_, err := db.AddStashes(stashes, items, bdb)
		if err != nil {
			t.Fatalf("failed to AddStashes, err=%s", err)
		}

		// This needs to be done AFTER the database has been populated
		query, league := MultiModSearchToIndexQuery(search, bdb, t)

		// Run the search and translate into items
		ids, err := query.Run(bdb)
		if err != nil {
			t.Fatalf("failed to run query, err=%s", err)
		}

		foundItems := QueryResultsToItems(ids, league, bdb, t)
		if len(foundItems) != len(expected) {
			t.Logf("expected %d items, found %d items",
				len(expected), len(foundItems))
		}
	})

	// Keep the items we expect here.
	//
	// This will have items added between sub-tests when the database
	// is being manipulated.
	expected = []db.GGGID{
		db.GGGIDFromUID("3d474bb6f4d2b3bf86c0911aac89b5c50bef1d556240f745936df3b7d78a1db1"),
	}

	t.Run("3ItemsRemoved", func(t *testing.T) {
		stashes, items := GetTestStashUpdate("singleStash.json",
			bdb, t)

		_, err := db.AddStashes(stashes, items, bdb)
		if err != nil {
			t.Fatalf("failed to AddStashes, err=%s", err)
		}

		// This needs to be done AFTER the database has been populated
		query, league := MultiModSearchToIndexQuery(search, bdb, t)

		// Run the search and translate into items
		ids, err := query.Run(bdb)
		if err != nil {
			t.Fatalf("failed to run query, err=%s", err)
		}

		// Two ways this can fail
		// 1. we get back more items than what we know we should get
		// 2. we fail to find an ID, in that case the QueryResultsToItems fails
		foundItems := QueryResultsToItems(ids, league, bdb, t)
		if len(foundItems) != len(expected) {
			t.Fatalf("expected %d items, found %d items",
				len(expected), len(foundItems))
		}
	})

}
