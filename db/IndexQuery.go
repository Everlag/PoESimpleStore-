package db

import (
	"github.com/boltdb/bolt"
	"github.com/pkg/errors"
)

var indexSetsPool = NewIDMapPool(10)

// LookupItemsMultiModStrideLength determines how many items
// is included in a stride of LookupItemsMultiMod.
//
// Longer strides mean fewer intersections but more potentially useless
// item mods checked.
const LookupItemsMultiModStrideLength = 32

// IndexQuery represents a query running over established indices
//
// An IndexQuery can be rerun by reinitializing the ctx; this typically
// happens when the query is Run.
type IndexQuery struct {
	// Type and flavor of the item we're looking up
	rootType, rootFlavor StringHeapID
	// Mods we are looking for
	mods []StringHeapID
	// Minimum mod values we are required to find
	//
	// Positionally related to mods
	minModValues []uint16
	// League we are searching for
	league LeagueHeapID
	// How many items we are limited to finding
	maxDesired int
	// Context necessary for a query to run
	ctx *indexQueryContext
}

// indexQueryContext represents the necessary transaction-dependent
// context for an IndexQuery to run.
type indexQueryContext struct {
	tx           *bolt.Tx
	validCursors int
	// Cursors we iterate over to perform our query
	//
	// These are positionally related to the parent's IndexQuery.mods
	cursors []*bolt.Cursor
	set     map[ID]int
	result  []ID
}

// Remove a given cursor from tracking on the context
func (ctx *indexQueryContext) removeCursor(index int) {
	ctx.cursors[index] = nil
	ctx.validCursors--
}

// NewIndexQuery returns an IndexQuery with no context
func NewIndexQuery(rootType, rootFlavor StringHeapID,
	mods []StringHeapID, minModValues []uint16,
	league LeagueHeapID,
	maxDesired int) IndexQuery {

	minModValuesScaled := make([]uint16, len(minModValues))
	for i, minValue := range minModValues {
		minModValuesScaled[i] = minValue * ItemModAverageScaleFactor
	}

	return IndexQuery{
		rootType, rootFlavor,
		mods, minModValuesScaled,
		league, maxDesired, nil,
	}

}

// initContext prepares transaction dependent context for an IndexQuery
func (q *IndexQuery) initContext(tx *bolt.Tx) error {

	// Make a place to keep our cursors
	//
	// NOTE: a cursor can be nil to indicate it should not be queried
	cursors := make([]*bolt.Cursor, len(q.mods))

	// Keep track of how many cursors are valid,
	// this will let us know when we've exhausted our data
	validCursors := len(cursors)

	// Collect our buckets for each mod and establish cursors
	for i, mod := range q.mods {
		itemModBucket, err := getItemModIndexBucketRO(q.rootType, q.rootFlavor,
			mod, q.league, tx)
		if err != nil {
			return errors.Errorf("faield to get item mod index bucket, mod=%d err=%s",
				mod, err)
		}
		cursors[i] = itemModBucket.Cursor()
	}

	// Create our item sets
	prealloc := LookupItemsMultiModStrideLength * 3 * len(q.mods)
	set := make(map[ID]int, prealloc)

	// And where we store our final result, preallocated but zero length
	result := make([]ID, 0, q.maxDesired)

	q.ctx = &indexQueryContext{
		tx, validCursors, cursors, set, result,
	}

	return nil
}

// clearContext removes transaction dependent context from IndexQuery
func (q *IndexQuery) clearContext() {
	q.ctx = nil
}

// registerID registers an ID as having matched a mod.
//
// When an ID has matched all mods, it is removed and added to the result
func (q *IndexQuery) registerID(id ID) {
	shared, ok := q.ctx.set[id]
	if !ok {
		shared = 0
	}
	shared++
	q.ctx.set[id] = shared
	if shared >= len(q.mods) {
		q.ctx.result = append(q.ctx.result, id)
		delete(q.ctx.set, id)
	}
}

// checkPair determines if a pair is acceptable for our query
// and modifes the associated modIndex Cursor appropriately.
//
// Returns the number of item IDs handled. Zero implies
// the cursor is no longer valid.
func (q *IndexQuery) checkPair(k, v []byte, modIndex int) (int, error) {
	// Grab the value
	values, err := decodeModIndexKey(k)
	if err != nil {
		return 0,
			errors.Wrap(err, "failed to decode mod index key")
	}
	if len(values) == 0 {
		return 0,
			errors.Errorf("decoded item mod index key to no values, key=%v", k)
	}

	// Ensure the mod is the correct value
	valid := values[0] >= q.minModValues[modIndex]
	var idCount int
	if valid {
		wrapped := IndexEntry(v)
		wrapped.ForEachID(q.registerID)
	} else {
		// Remove from cursors we're interested in
		q.ctx.removeCursor(modIndex)
	}

	return idCount, nil
}

// stide performs a single stride on the query, filling sets on ctx
// as appropriate and also invalidates cursors which are useless
func (q *IndexQuery) stride() error {

	// Go over each cursor
	for i, c := range q.ctx.cursors {
		// Handle nil cursor indicating that mod
		// has no more legitimate values
		if c == nil {
			continue
		}

		// Perform the actual per-cursor stride
		for index := 0; index < LookupItemsMultiModStrideLength; {

			// Grab a pair
			k, v := c.Prev()
			// Ignore nested buckets but also
			// handle reaching the start of the bucket
			if k == nil {
				// Both nil means we're done
				if v == nil {
					q.ctx.removeCursor(i)
					break
				}
				continue
			}
			var err error
			countFound, err := q.checkPair(k, v, i)
			if err != nil {
				return errors.Wrap(err, "failed to check value pair")
			}

			// If its not a valid pair, we're done iterating on this cursor
			if countFound < 1 {
				break
			}
			index += countFound
		}
	}
	return nil
}

// Run initialises transaction context for a query and attempts
// to find desired items.
func (q *IndexQuery) Run(db *bolt.DB) ([]ID, error) {

	// Always clear the context when we exit
	defer q.clearContext()

	err := db.View(func(tx *bolt.Tx) error {

		err := q.initContext(tx)
		if err != nil {
			return errors.New("failed to initialize query context")
		}

		// Set all of our cursors to be at their ends
		for i, c := range q.ctx.cursors {
			// Set to last
			k, v := c.Last()
			// Ignore nested buckets
			if k == nil {
				continue
			}
			// Check the pair, we only care about possible errors here
			if _, err := q.checkPair(k, v, i); err != nil {
				return errors.Wrap(err, "failed to check value in bucekt")
			}
		}

		// Perform our strides to search
		var foundIDs int
		for foundIDs < q.maxDesired && q.ctx.validCursors > 0 {
			// Iterate for a stride
			err := q.stride()
			if err != nil {
				return errors.Wrap(err, "failed a stride")
			}

			// foundIDs = q.intersectIDSets(nil)
			foundIDs = len(q.ctx.result)
		}

		return nil
	})

	return q.ctx.result, err
}
