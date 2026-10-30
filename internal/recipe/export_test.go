package recipe

import (
	"maps"
	"slices"
)

func StepTypes() []string { return slices.Sorted(maps.Keys(params)) }
