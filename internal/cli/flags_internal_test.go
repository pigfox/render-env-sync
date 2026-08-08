package cli

import (
	"flag"
	"io"
	"testing"
)

// boolFlag is the interface the flag package uses to mark flags that do not
// consume the following argument. Every flag that does *not* implement it
// takes a value.
type boolFlag interface {
	IsBoolFlag() bool
}

// TestValueFlagsMatchesTheFlagSet fails the build when someone adds a flag
// that takes a value and forgets to register it in valueFlags.
//
// The consequence of that drift is silent, not loud. permute only knows to
// skip past a flag's value for the names in valueFlags; an unregistered value
// flag appearing after a positional swallows the positional instead:
//
//	renv push proj/dev --service srv-x
//	  → flags=["--service"], positional=["proj/dev","srv-x"]
//	  → flag.Parse reads --service=proj/dev, no error
//
// A push aimed at the wrong target, reported as success. Hence a test rather
// than a comment.
func TestValueFlagsMatchesTheFlagSet(t *testing.T) {
	fs := newFlagSet("audit", &options{}, io.Discard)

	registered := map[string]bool{}
	fs.VisitAll(func(f *flag.Flag) {
		bf, ok := f.Value.(boolFlag)
		takesValue := !ok || !bf.IsBoolFlag()
		registered[f.Name] = takesValue

		if takesValue && !valueFlags[f.Name] {
			t.Errorf("flag -%s takes a value but is missing from valueFlags; "+
				"permute will mis-sort it when it appears after a positional", f.Name)
		}
		if !takesValue && valueFlags[f.Name] {
			t.Errorf("flag -%s is boolean but is listed in valueFlags; "+
				"permute will swallow the argument after it", f.Name)
		}
	})

	for name := range valueFlags {
		if _, defined := registered[name]; !defined {
			t.Errorf("valueFlags lists -%s, which is not a registered flag", name)
		}
	}

	// Sanity check on the detection itself: if this stops finding the one
	// known value flag, the boolFlag assertion has silently stopped working
	// and every assertion above would pass vacuously.
	if !registered["config"] {
		t.Fatal("config was not detected as taking a value; boolFlag detection is broken")
	}
	if registered["apply"] {
		t.Fatal("apply was detected as taking a value; boolFlag detection is broken")
	}
}
