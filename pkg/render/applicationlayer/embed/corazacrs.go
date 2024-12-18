package embed

import (
	"io/fs"

	coreruleset "github.com/corazawaf/coraza-coreruleset/v4"
)

var RulesetRulesFS *fsWrapper

func init() {
	var err error
	rulesFs, err := fs.Sub(coreruleset.FS, "@owasp_crs")
	if err != nil {
		panic(err)
	}
	RulesetRulesFS = NewWrappedFS(rulesFs)
}
