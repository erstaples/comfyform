package codegen

import (
	"sort"
	"strings"
	"unicode"

	"github.com/erstaples/comfyform/schema"
)

// GoName converts "ckpt_name" -> "CkptName", "LATENT" -> "Latent",
// "Image Film Grain (WAS)" -> "ImageFilmGrainWAS".
func GoName(s string) string {
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r)) || r > unicode.MaxASCII
	})
	var b strings.Builder
	for _, p := range parts {
		if isUpper(p) && len(p) > 3 {
			p = strings.ToLower(p) // LATENT -> Latent, but keep VAE, CLIP, WAS
		}
		b.WriteString(strings.ToUpper(p[:1]) + p[1:])
	}
	out := b.String()
	if out == "" {
		out = "X"
	}
	if unicode.IsDigit(rune(out[0])) {
		out = "N" + out
	}
	return out
}

func isUpper(s string) bool {
	hasLetter := false
	for _, r := range s {
		if unicode.IsLower(r) {
			return false
		}
		if unicode.IsLetter(r) {
			hasLetter = true
		}
	}
	return hasLetter
}

// lowerFirst is for local variable names in imported code.
func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	// "VAEDecode" -> "vaeDecode", "KSampler" -> "kSampler"
	r := []rune(s)
	i := 0
	for i < len(r) && unicode.IsUpper(r[i]) {
		i++
	}
	if i > 1 && i < len(r) {
		i-- // keep the start of the next word capitalized
	}
	if i == 0 {
		i = 1
	}
	return strings.ToLower(string(r[:i])) + string(r[i:])
}

// TypeName is the marker type for a socket type: "MODEL" -> "MODEL",
// "UPSCALE_MODEL" -> "UPSCALE_MODEL". Returns "" for any-typed/union sockets,
// which take graph.AnyRef instead.
func TypeName(t string) string {
	if t == "*" || t == "" || strings.Contains(t, ",") {
		return ""
	}
	var b strings.Builder
	for _, r := range t {
		if r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_') {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	s := b.String()
	if unicode.IsDigit(rune(s[0])) {
		s = "T" + s
	}
	return s
}

// Names is the resolved naming for one class; shared by codegen and importer
// so imported code always matches generated bindings.
type Names struct {
	Func    string
	Args    string
	Out     string
	Inputs  map[string]string // input name -> field
	Outputs []string          // output index -> field
}

// Resolve assigns unique Go identifiers to every class in the schema.
func Resolve(s *schema.Schema) map[string]*Names {
	used := map[string]bool{"Bool": true}
	// marker types claim names first
	for _, cn := range s.Names() {
		c := s.Classes[cn]
		for _, in := range c.Inputs {
			if in.Kind == schema.KindLink {
				if t := TypeName(in.Type); t != "" {
					used[t] = true
				}
			}
		}
		for _, o := range c.Outputs {
			if t := TypeName(o.Type); t != "" {
				used[t] = true
			}
		}
	}
	unique := func(base string) string {
		n := base
		for i := 2; used[n] || used[n+"Args"] || used[n+"Out"]; i++ {
			n = base + itoa(i)
		}
		used[n], used[n+"Args"], used[n+"Out"] = true, true, true
		return n
	}
	// Classes whose name is already their Go name claim it first, so a
	// custom "Checkpoint Loader (Simple)" can't take CheckpointLoaderSimple
	// from core, and installing a pack doesn't rename existing bindings.
	order := s.Names()
	sort.SliceStable(order, func(i, j int) bool {
		return GoName(order[i]) == order[i] && GoName(order[j]) != order[j]
	})
	out := map[string]*Names{}
	for _, cn := range order {
		c := s.Classes[cn]
		fn := unique(GoName(cn))
		nm := &Names{Func: fn, Args: fn + "Args", Out: fn + "Out", Inputs: map[string]string{}}
		fields := map[string]bool{"Raw": true}
		for _, in := range c.Inputs {
			f := GoName(in.Name)
			for i := 2; fields[f]; i++ {
				f = GoName(in.Name) + itoa(i)
			}
			fields[f] = true
			nm.Inputs[in.Name] = f
		}
		ofields := map[string]bool{"Node": true}
		for _, o := range c.Outputs {
			base := GoName(o.Name)
			if o.Name == "*" {
				base = "Any"
			}
			f := base
			for i := 2; ofields[f]; i++ {
				f = base + itoa(i)
			}
			ofields[f] = true
			nm.Outputs = append(nm.Outputs, f)
		}
		out[cn] = nm
	}
	return out
}

func itoa(i int) string {
	const d = "0123456789"
	if i < 10 {
		return d[i : i+1]
	}
	return itoa(i/10) + d[i%10:i%10+1]
}
