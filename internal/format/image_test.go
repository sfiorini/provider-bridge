package format

import "testing"

func TestSplitImageSource(t *testing.T) {
	tests := []struct {
		name        string
		imageData   string
		mediaType   string
		wantMedia   string
		wantPayload string
		wantURL     bool
		wantOK      bool
	}{
		{
			name:        "data URL is split into media type and raw base64",
			imageData:   "data:image/png;base64,AAAA",
			wantMedia:   "image/png",
			wantPayload: "AAAA",
			wantOK:      true,
		},
		{
			name:        "data URL header wins over empty MediaType",
			imageData:   "data:image/jpeg;base64,BBBB",
			wantMedia:   "image/jpeg",
			wantPayload: "BBBB",
			wantOK:      true,
		},
		{
			name:        "data URL header is preferred over a conflicting MediaType",
			imageData:   "data:image/webp;base64,CCCC",
			mediaType:   "image/png",
			wantMedia:   "image/webp",
			wantPayload: "CCCC",
			wantOK:      true,
		},
		{
			name:        "data URL without a media type falls back to MediaType",
			imageData:   "data:;base64,DDDD",
			mediaType:   "image/gif",
			wantMedia:   "image/gif",
			wantPayload: "DDDD",
			wantOK:      true,
		},
		{
			name:        "raw base64 passes through with MediaType",
			imageData:   "EEEE",
			mediaType:   "image/png",
			wantMedia:   "image/png",
			wantPayload: "EEEE",
			wantOK:      true,
		},
		{
			name:        "raw base64 without MediaType defaults to image/png",
			imageData:   "FFFF",
			wantMedia:   "image/png",
			wantPayload: "FFFF",
			wantOK:      true,
		},
		{
			name:        "https URL is reported as a URL",
			imageData:   "https://example.com/a.png",
			wantMedia:   "",
			wantPayload: "https://example.com/a.png",
			wantURL:     true,
			wantOK:      true,
		},
		{
			name:        "http URL is reported as a URL",
			imageData:   "http://example.com/a.png",
			wantPayload: "http://example.com/a.png",
			wantURL:     true,
			wantOK:      true,
		},
		{
			name:      "surrounding whitespace is trimmed",
			imageData: "  data:image/png;base64,GGGG  ",
			wantMedia: "image/png", wantPayload: "GGGG", wantOK: true,
		},
		{
			name:        "malformed data URL without comma is treated as raw base64",
			imageData:   "data:image/png;base64",
			wantMedia:   "image/png",
			wantPayload: "data:image/png;base64",
			wantOK:      true,
		},
		{
			name:      "empty ImageData is not ok",
			imageData: "",
			wantOK:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotMedia, gotPayload, gotURL, gotOK := SplitImageSource(CoreContentBlock{
				Type:      "image",
				ImageData: tt.imageData,
				MediaType: tt.mediaType,
			})
			if gotOK != tt.wantOK {
				t.Fatalf("ok = %v, want %v", gotOK, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			if gotMedia != tt.wantMedia {
				t.Errorf("mediaType = %q, want %q", gotMedia, tt.wantMedia)
			}
			if gotPayload != tt.wantPayload {
				t.Errorf("payload = %q, want %q", gotPayload, tt.wantPayload)
			}
			if gotURL != tt.wantURL {
				t.Errorf("isURL = %v, want %v", gotURL, tt.wantURL)
			}
		})
	}
}
