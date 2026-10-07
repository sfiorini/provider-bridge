// Package format: Core image source normalisation.
//
// Core "image" content blocks are produced by several inbound adapters, and
// they do not agree on how ImageData is spelled:
//
//   - the anthropic inbound carries RAW base64 in ImageData with MediaType set;
//   - the chat / responses inbounds carry the client's `image_url.url` verbatim,
//     which is usually a full data URL ("data:image/png;base64,AAAA");
//   - the visual orchestrator may emit either a raw data URL or an HTTP(S) URL.
//
// Upstream adapters therefore must NOT feed ImageData straight into a
// protocol-specific "base64" field. Doing so forwards the literal string
// "data:image/png;base64,AAAA" as if it were base64 payload, and upstreams that
// validate it (Anthropic — used by DeepSeek) reject the request with
// `base64 decode error`. SplitImageSource is the single place that normalises a
// Core image block for those upstreams.
package format

import "strings"

// DefaultImageMediaType is assumed when an image block carries neither a data
// URL header nor an explicit MediaType.
const DefaultImageMediaType = "image/png"

// SplitImageSource normalises a Core "image" content block into the two shapes
// every upstream protocol can express:
//
//   - isURL == true  → payload is a plain http(s) URL (Anthropic `url` source);
//   - isURL == false → payload is RAW base64 (Anthropic/Google/DeepSeek
//     `base64` source), with mediaType describing it.
//
// A "data:<mediatype>;base64,<payload>" ImageData is split, preferring the
// media type embedded in the header and falling back to MediaType. A bare
// http(s) URL keeps MediaType for reference but is reported as a URL. Anything
// else is treated as raw base64.
//
// ok is false only when the block carries no image data at all.
func SplitImageSource(b CoreContentBlock) (mediaType, payload string, isURL bool, ok bool) {
	raw := strings.TrimSpace(b.ImageData)
	if raw == "" {
		return "", "", false, false
	}

	if strings.HasPrefix(raw, "data:") {
		header, data, found := strings.Cut(raw, ",")
		if !found {
			// Malformed data URL: no comma separator. Treat as raw base64.
			return mediaTypeOrDefault(b.MediaType), raw, false, true
		}
		mt := strings.TrimPrefix(header, "data:")
		if semi := strings.IndexByte(mt, ';'); semi >= 0 {
			mt = mt[:semi]
		}
		if mt == "" {
			mt = mediaTypeOrDefault(b.MediaType)
		}
		return mt, data, false, true
	}

	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		return b.MediaType, raw, true, true
	}

	return mediaTypeOrDefault(b.MediaType), raw, false, true
}

func mediaTypeOrDefault(mediaType string) string {
	if strings.TrimSpace(mediaType) == "" {
		return DefaultImageMediaType
	}
	return mediaType
}
