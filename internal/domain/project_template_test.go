package domain

import (
	"reflect"
	"slices"
	"sort"
	"testing"
	"time"
)

func TestValidTemplateID(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want bool
	}{
		{"simple slug", "materials-discovery", true},
		{"digits ok", "m2", true},
		{"digits and dashes", "v1-beta-2", true},
		{"single char", "x", true},
		{"64 chars ok", "a123456789012345678901234567890123456789012345678901234567890123", true},
		{"empty", "", false},
		{"leading dash", "-abc", false},
		{"uppercase", "Materials", false},
		{"underscore", "a_b", false},
		{"dot", "a.b", false},
		{"space", "a b", false},
		{"unicode", "材料", false},
		{"65 chars too long", "a1234567890123456789012345678901234567890123456789012345678901234", false},
		{"dash only", "-", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ValidTemplateID(tt.id); got != tt.want {
				t.Errorf("ValidTemplateID(%q) = %v, want %v", tt.id, got, tt.want)
			}
		})
	}
}

func TestValidTemplateVersion(t *testing.T) {
	tests := []struct {
		name string
		v    string
		want bool
	}{
		{"v1", "v1", true},
		{"semver", "1.2.3", true},
		{"prerelease", "v2-beta.1", true},
		{"underscore", "v1_2", true},
		{"single char", "a", true},
		{"64 chars ok", "a123456789012345678901234567890123456789012345678901234567890123", true},
		{"empty", "", false},
		{"space", "v 1", false},
		{"slash", "v/1", false},
		{"unicode", "v一", false},
		{"65 chars too long", "a1234567890123456789012345678901234567890123456789012345678901234", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ValidTemplateVersion(tt.v); got != tt.want {
				t.Errorf("ValidTemplateVersion(%q) = %v, want %v", tt.v, got, tt.want)
			}
		})
	}
}

// TestTemplateInstantiationIsRecordOnly guards the design property behind
// acceptance criterion 3 (模板升级不会自动改已有项目): the instantiation is
// a recorded fact — the template id + version, the display name it was
// created under, and the provenance columns — carrying NO control field.
// There is nothing in the type a later task could read back to re-apply a
// template's defaults, so a catalog upgrade has no handle on an existing
// project.
//
// The guard is the TYPE'S SHAPE, read with reflection, because that shape
// is the only thing that can change without this test noticing. Asserting
// values on a literal built inside this test would hold for every
// definition of the type — it would stay green with a
// TemplateProjectDefaults field added, while its name claimed the
// opposite. The field set and every field's type are pinned below; adding,
// removing or retyping a field (a defaults struct, a re-apply flag) turns
// this test red and forces the change to be deliberate.
func TestTemplateInstantiationIsRecordOnly(t *testing.T) {
	// wantFields is the pinned shape, sorted: who recorded what, on which
	// project, when.
	wantFields := []string{
		"CreatedAt",       // when the project was created from the template
		"CreatedBy",       // which actor the project was created by
		"ID",              // the record's own id
		"ProjectID",       // the project the record belongs to
		"TemplateID",      // 记录 template id
		"TemplateName",    // the display name at record time
		"TemplateVersion", // 记录 template version
	}
	typ := reflect.TypeOf(TemplateInstantiation{})
	gotFields := make([]string, 0, typ.NumField())
	for i := range typ.NumField() {
		gotFields = append(gotFields, typ.Field(i).Name)
	}
	sort.Strings(gotFields)
	if !slices.Equal(gotFields, wantFields) {
		t.Fatalf("TemplateInstantiation fields = %v, want exactly %v — the record is a provenance fact: any field beyond this set (a defaults struct, a re-apply flag) is control a later task could read back and re-apply, which is what a template must never keep (acceptance criterion 3)", gotFields, wantFields)
	}

	// Every field is plain data: the recorded timestamp or a string. The
	// field-set check above catches a RENAMED control field; this one
	// catches a RETYPED one — TemplateName becoming a defaults struct keeps
	// its name and would otherwise slip through the set comparison.
	plain := []reflect.Type{reflect.TypeOf(""), reflect.TypeOf(time.Time{})}
	for i := range typ.NumField() {
		f := typ.Field(i)
		if !slices.Contains(plain, f.Type) {
			t.Fatalf("TemplateInstantiation.%s has type %s, want plain data (%v) — a record field carrying a struct, pointer, slice or map is something a later task could read back", f.Name, f.Type, plain)
		}
	}
}
