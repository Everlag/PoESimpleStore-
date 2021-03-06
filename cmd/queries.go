package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/Everlag/poeitemstore/stash"
	"github.com/pkg/errors"
)

// MultiModSearch specifies a search to perform for items
type MultiModSearch struct {
	MaxDesired int
	RootType   string
	RootFlavor string
	League     string
	Mods       []string
	MinValues  []uint16
}

func (search *MultiModSearch) String() string {
	modPrints := make([]string, len(search.Mods))
	var modString string
	if len(search.Mods) != len(search.MinValues) {
		modString = "invalid mods: len(Mods) != len(MinValues)"
	} else {
		for i, mod := range search.Mods {
			modPrints[i] = fmt.Sprintf("%s: %d", mod, search.MinValues[i])
		}
		if len(modPrints) == 0 {
			modString = "no mods present"
		}
		modString = strings.Join(modPrints, "\n")
	}
	return fmt.Sprintf(`RootType: %s, RootFlavor: %s,
League: %s, MaxDesired: %d
%s`,
		search.RootType, search.RootFlavor,
		search.League, search.MaxDesired, modString)
}

// Clone copies the MultiModSearch to a copy that can be mutated
// without effecting the original
func (search MultiModSearch) Clone() MultiModSearch {
	// Shallow copy for primitive fields
	clone := search

	// Deep copy of non-primitive fields
	clone.Mods = make([]string, len(search.Mods))
	copy(clone.Mods, search.Mods)
	clone.MinValues = make([]uint16, len(search.MinValues))
	copy(clone.MinValues, search.MinValues)

	return clone
}

// Satisfies determines if a provided set of Items is acceptable
// under the query
func (search *MultiModSearch) Satisfies(result []stash.Item) bool {

	// Invalid search means we panic
	if len(search.Mods) != len(search.MinValues) {
		panic("invalid MultiModSearch, mismatched lengths of Mods to MinValues")
	}

	// Easy lookup for minimum values
	required := make(map[string]uint16)
	for i, mod := range search.Mods {
		required[mod] = search.MinValues[i]
	}

	requiredSatisfiedMods := len(search.Mods)

	// Ensure each item has mods to satisfy this query.
	for _, item := range result {
		modsSatisfied := 0

		mods := item.GetMods()
		for _, mod := range mods {
			min, ok := required[string(mod.Template)]
			if !ok {
				continue
			}
			// TODO: change the way we handle multi-value mods
			if min <= mod.Values[0] {
				modsSatisfied++
			}
		}
		if modsSatisfied < requiredSatisfiedMods {
			return false
		}
	}

	return true
}

// FetchMultiModSearch returns a MultiModSearch deserialized
// from the provided path on disk
func FetchMultiModSearch(path string) (*MultiModSearch, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.Wrap(err, "failed to open file")
	}
	decoder := json.NewDecoder(f)
	var search MultiModSearch
	err = decoder.Decode(&search)
	if err != nil {
		return nil, errors.Wrap(err, "failed to read query")
	}

	return &search, nil
}
