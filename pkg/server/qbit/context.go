package qbit

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"

	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/arr"
)

type contextKey string

const (
	categoryKey contextKey = "category"
	hashesKey   contextKey = "hashes"
	arrKey      contextKey = "arr"
)

func getCategory(ctx context.Context) string {
	if category, ok := ctx.Value(categoryKey).(string); ok {
		return category
	}
	return ""
}

func getHashes(ctx context.Context) []string {
	if hashes, ok := ctx.Value(hashesKey).([]string); ok {
		return hashes
	}
	return nil
}

func getArrFromContext(ctx context.Context) arr.Arr {
	instance, _ := ctx.Value(arrKey).(arr.Arr)
	return instance
}

// decodeAuthHeader reads an Authorization header: Basic credentials, or a
// Bearer API key (returned as the password, with no username). A missing
// header yields empty credentials and no error.
func decodeAuthHeader(header string) (string, string, error) {
	scheme, encodedToken, ok := strings.Cut(header, " ")
	if !ok || strings.Contains(encodedToken, " ") {
		return "", "", nil
	}
	if strings.EqualFold(scheme, "Bearer") {
		return "", encodedToken, nil
	}
	if !strings.EqualFold(scheme, "Basic") {
		return "", "", fmt.Errorf("unsupported authorization scheme %q", scheme)
	}

	bytes, err := base64.StdEncoding.DecodeString(encodedToken)
	if err != nil {
		return "", "", err
	}

	bearer := string(bytes)

	before, after, ok := strings.CutLast(bearer, ":")
	if !ok {
		// strings.LastIndex returns -1 when the substring is absent; without
		// this guard `bearer[:colonIndex]` would panic with
		// "slice bounds out of range [:-1]". Triggers on any Authorization
		// header whose decoded base64 payload contains no ':' separator
		// (e.g. an empty payload, or garbage bytes that decode but lack a
		// 'user:pass' shape).
		return "", "", fmt.Errorf("malformed credentials: missing colon separator")
	}
	username := before
	password := after

	if username == "" || password == "" {
		return username, password, fmt.Errorf("empty username or password")
	}

	return strings.TrimSpace(username), strings.TrimSpace(password), nil
}

func (q *QBit) categoryContext(next http.Handler) http.Handler {
	// Print full URL for debugging

	// Try to get category from URL query first
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Print request method and URL
		category := strings.Trim(r.URL.Query().Get("category"), "")
		if category == "" {
			// GetReader from form
			_ = r.ParseForm()
			category = r.Form.Get("category")
			if category == "" {
				// GetReader from multipart form
				_ = utils.ParseBoundedMultipartForm(w, r, maxRequestBody, multipartMemory)
				category = r.FormValue("category")
			}
		}
		ctx := context.WithValue(r.Context(), categoryKey, strings.TrimSpace(category))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// authContext creates a middleware that extracts the Arr host and token from the Authorization header
// and adds it to the request context.
// This is used to identify the Arr instance for the utils.
// Only a valid host and token will be added to the context/config. The rest are manual.
func (q *QBit) authContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, err := q.getUsernameAndPassword(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		category := getCategory(r.Context())
		a, err := q.authenticate(r.Context(), category, username, password)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), arrKey, a)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (q *QBit) getUsernameAndPassword(r *http.Request) (string, string, error) {
	// Try to get from authorization header
	username, password, err := decodeAuthHeader(r.Header.Get("Authorization"))
	if err == nil && password != "" {
		return username, password, nil
	}
	// Try to get from cookie
	sid, err := r.Cookie("sid")
	if err != nil {
		// try SID
		sid, err = r.Cookie("SID")
	}
	if err == nil {
		username, password, err = extractFromSID(q.config.Get().SecretKey(), sid.Value)
		if err != nil {
			return "", "", err
		}
	}
	return username, password, nil
}

func (q *QBit) authenticate(ctx context.Context, category, username, password string) (arr.Arr, error) {
	cfg := q.config.Get()
	instance, known := q.manager.Arr().Get(category)
	if !known {
		// Not in the registry yet: inherit download_uncached from a matching
		// config entry so SendToDebrid does not fall back to the provider.
		instance = arr.Arr{Name: category, Source: arr.SourceAuto}
		for _, configured := range cfg.Arrs {
			if configured.Name == category {
				instance.DownloadUncached = configured.DownloadUncached
				break
			}
		}
	}
	if cfg.UseAuth {
		if q.config.Get().VerifyAuth(username, password) || q.config.Get().VerifyToken(password) {
			return instance, nil
		}
		if matched, ok := q.manager.Arr().MatchCredentials(category, username, password); ok {
			return matched, nil
		}
		return arr.Arr{}, fmt.Errorf("unauthorized: invalid credentials")
	}

	validated := false
	kind := instance.Type
	if username != "" && password != "" {
		candidate := instance
		candidate.Host = username
		candidate.Token = password
		candidate.Source = arr.SourceAuto
		probed, err := q.manager.Arr().Probe(ctx, candidate)
		validated = err == nil
		if validated {
			kind = probed
		}
	}

	if validated && instance.Source == arr.SourceAuto {
		instance.Host = username
		instance.Token = password
		instance.Type = kind
		q.manager.Arr().AddOrUpdate(instance)
	}
	return instance, nil
}

func createSID(secretKey, username, password string) string {
	// Create a verification hash
	combined := fmt.Sprintf("%s|%s", username, password)
	hash := sha256.Sum256([]byte(combined + secretKey))
	hashStr := hex.EncodeToString(hash[:])[:16] // First 16 chars
	// Base64 encode
	return base64.URLEncoding.EncodeToString(fmt.Appendf(nil, "%s|%s", combined, hashStr))
}

func extractFromSID(secretKey, sid string) (string, string, error) {
	// Decode base64
	decoded, err := base64.URLEncoding.DecodeString(sid)
	if err != nil {
		return "", "", fmt.Errorf("invalid SID format")
	}

	// Split into parts: username:password:hash
	username, rest, ok := strings.Cut(string(decoded), "|")
	password, providedHash, ok2 := strings.Cut(rest, "|")
	if !ok || !ok2 || strings.Contains(providedHash, "|") {
		return "", "", fmt.Errorf("invalid SID structure")
	}

	// Verify hash
	combined := fmt.Sprintf("%s|%s", username, password)
	expectedHash := sha256.Sum256([]byte(combined + secretKey))
	expectedHashStr := hex.EncodeToString(expectedHash[:])[:16]

	if providedHash != expectedHashStr {
		return "", "", fmt.Errorf("invalid SID signature")
	}

	return username, password, nil
}

func hashesContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		// qBittorrent takes several hashes as one pipe-separated value.
		var hashes []string
		for _, value := range r.Form["hashes"] {
			for hash := range strings.SplitSeq(value, "|") {
				if hash = strings.TrimSpace(hash); hash != "" {
					hashes = append(hashes, hash)
				}
			}
		}
		ctx := context.WithValue(r.Context(), hashesKey, hashes)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
