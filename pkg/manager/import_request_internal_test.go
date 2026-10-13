package manager

import "testing"

func TestImportRequestSkipMultiSeason(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		typ  ImportType
		skip bool
		want bool
	}{
		{"api default fans out", ImportTypeAPI, false, false},
		{"api explicit skip", ImportTypeAPI, true, true},
		{"qbit never fans out", ImportTypeQBit, false, true},
		{"sabnzbd never fans out", ImportTypeSABnzbd, false, true},
		{"watch never fans out", ImportTypeWatch, false, true},
		{"switcher never fans out", ImportSwitcher, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := &ImportRequest{Type: tt.typ, SkipMultiSeason: tt.skip}
			if got := r.skipMultiSeason(); got != tt.want {
				t.Fatalf("skipMultiSeason() = %v, want %v", got, tt.want)
			}
		})
	}
}
