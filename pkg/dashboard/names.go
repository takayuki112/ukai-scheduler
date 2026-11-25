package dashboard

import (
	"regexp"
	"strings"

	"github.com/takayuki112/ukai-scheduler/pkg/tiers"
)

var (
	brands  = regexp.MustCompile(`(?i)^((nvidia|tesla|geforce|quadro|rtx|gtx)[-_ ]+)+`)
	details = regexp.MustCompile(`(?i)[-_ ]+(\d+gb|sxm\d*|pcie|hbm\d*e?|nvl\d*|shared)\b`)
	memory  = regexp.MustCompile(`(?i)\d+gb`)
)

// KAI names a PodGroup pg-<name>-<UID>.
var podGroup = regexp.MustCompile(`/pg-(.+)-[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)

// jobName is the namespace and name of the Job, or whatever it is.
func jobName(id string) string { return podGroup.ReplaceAllString(id, "/$1") }

// shortNames shortens GPU types as NVIDIA's labels name them, such as NVIDIA-H100-80GB-HBM3
// to H100. Those that would be the same keep their memory, or else their whole name.
func shortNames(types []string) map[string]string {
	short := map[string]string{}
	count := map[string]int{}
	for _, t := range types {
		if _, done := short[t]; done {
			continue
		}
		s := t
		if t != anyType && t != unknownType {
			s = brands.ReplaceAllString(t, "")
			s = strings.TrimSpace(details.ReplaceAllString(s, ""))
			s = strings.NewReplacer("-Ti", "Ti", "-SUPER", "S", "-", " ", "_", " ").Replace(s)
			if s == "" {
				s = t
			}
		}
		short[t] = s
		count[s]++
	}
	again := map[string]int{}
	for t, s := range short {
		if count[s] > 1 {
			if m := memory.FindString(t); m != "" {
				short[t] = s + " " + strings.ToUpper(m)
			} else {
				short[t] = t
			}
		}
		again[short[t]]++
	}
	for t, s := range short {
		if again[s] > 1 {
			short[t] = t
		}
	}
	return short
}

// gpus is as in "2x H100".
func (v *View) gpus(n tiers.Units, types []string) string {
	names := make([]string, len(types))
	for i, t := range types {
		names[i] = v.name(t)
	}
	return n.String() + "x " + strings.Join(names, "+")
}

func (v *View) name(t string) string {
	if s := v.names[t]; s != "" {
		return s
	}
	return t
}
