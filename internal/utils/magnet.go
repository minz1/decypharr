package utils

import (
	"bufio"
	"bytes"
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/anacrolix/torrent/metainfo"

	"github.com/sirrobot01/decypharr/internal/logger"
)

var (
	hexRegex = regexp.MustCompile("^[0-9a-fA-F]{40}$")
)

// base32InfoHashLen is the length of a base32-encoded 20-byte SHA-1 infohash.
const base32InfoHashLen = 32

// Magnet is a parsed magnet link, optionally with the .torrent it came from.
type Magnet struct {
	Name     string `json:"name"`
	InfoHash string `json:"infoHash"`
	Size     int64  `json:"size"`
	Link     string `json:"link"`
	File     []byte `json:"-"`
}

func (m *Magnet) IsTorrent() bool {
	return m.File != nil
}

// stripTrackersFromMagnet removes trackers from a magnet and returns a modified copy.
func stripTrackersFromMagnet(mi metainfo.Magnet, fileType string) metainfo.Magnet {
	originalTrackerCount := len(mi.Trackers)
	if len(mi.Trackers) > 0 {
		log := logger.Default()
		mi.Trackers = nil
		log.Printf("Removed %d tracker URLs from %s", originalTrackerCount, fileType)
	}
	return mi
}

func GetMagnetFromFile(file io.Reader, filePath string, rmTrackerUrls bool) (*Magnet, error) {
	var (
		m         *Magnet
		err       error
		isTorrent = filepath.Ext(filePath) == ".torrent"
	)
	if isTorrent {
		torrentData, readAllErr := io.ReadAll(file)
		if readAllErr != nil {
			return nil, readAllErr
		}
		m, readAllErr = GetMagnetFromBytes(torrentData, rmTrackerUrls)
		if readAllErr != nil {
			return nil, readAllErr
		}
	} else {
		// .magnet file
		magnetLink := ReadMagnetFile(file)
		m, err = GetMagnetInfo(magnetLink, rmTrackerUrls)
		if err != nil {
			return nil, err
		}
	}
	uploadedName := strings.TrimSuffix(filepath.Base(filePath), filepath.Ext(filePath))
	if isTorrent {
		m.Name = uploadedName
		m.Link = SetMagnetDisplayName(m.Link, m.Name)
	} else if m.Name == "" {
		m.Name = uploadedName
		m.Link = SetMagnetDisplayName(m.Link, m.Name)
	}
	return m, nil
}

// GetMagnetFromUrl resolves a magnet link, or downloads a .torrent over HTTP(S).
//
//nolint:revive,staticcheck // var-naming/ST1003: exported name used by pkg/server; rename to GetMagnetFromURL is a cross-area change
func GetMagnetFromUrl(url string, rmTrackerUrls bool) (*Magnet, error) {
	if strings.HasPrefix(url, "magnet:") {
		return GetMagnetInfo(url, rmTrackerUrls)
	} else if strings.HasPrefix(url, "http") {
		return OpenMagnetHTTPURL(url, rmTrackerUrls)
	}
	return nil, fmt.Errorf("invalid url")
}

func GetMagnetFromBytes(torrentData []byte, rmTrackerUrls bool) (*Magnet, error) {
	// Create a scanner to read the file line by line
	mi, err := metainfo.Load(bytes.NewReader(torrentData))
	if err != nil {
		return nil, err
	}

	hash := mi.HashInfoBytes()
	infoHash := hash.HexString()
	info, err := mi.UnmarshalInfo()
	if err != nil {
		return nil, err
	}
	//nolint:staticcheck // SA1019: MagnetV2 adds btmh/ws params and reorders trackers; debrid APIs get the v1 magnet on purpose
	magnetMeta := mi.Magnet(&hash, &info)
	if rmTrackerUrls {
		magnetMeta = stripTrackersFromMagnet(magnetMeta, "torrent file")
	}
	magnet := &Magnet{
		InfoHash: infoHash,
		Name:     info.Name,
		Size:     info.Length,
		Link:     magnetMeta.String(),
		File:     torrentData,
	}
	return magnet, nil
}

func ReadMagnetFile(file io.Reader) string {
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		content := scanner.Text()
		if content != "" {
			return content
		}
	}

	// Check for any errors during scanning
	if err := scanner.Err(); err != nil {
		log := logger.Default()
		log.Println("Error reading file:", err)
	}
	return ""
}

// OpenMagnetHTTPURL downloads a .torrent file and converts it to a Magnet.
func OpenMagnetHTTPURL(magnetLink string, rmTrackerUrls bool) (*Magnet, error) {
	resp, err := fetch(magnetLink)
	if err != nil {
		return nil, fmt.Errorf("error making GET request: %w", err)
	}
	defer resp.Body.Close()
	torrentData, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading response body: %w", err)
	}
	return GetMagnetFromBytes(torrentData, rmTrackerUrls)
}

func GetMagnetInfo(magnetLink string, rmTrackerUrls bool) (*Magnet, error) {
	if magnetLink == "" {
		return nil, fmt.Errorf("error getting magnet from file")
	}

	mi, err := metainfo.ParseMagnetUri(magnetLink)
	if err != nil {
		return nil, fmt.Errorf("error parsing magnet link: %w", err)
	}

	// Strip all announce URLs if requested
	if rmTrackerUrls {
		mi = stripTrackersFromMagnet(mi, "magnet link")
	}

	btih := mi.InfoHash.HexString()
	dn := mi.DisplayName

	// Reconstruct the magnet link using the (possibly modified) spec
	finalLink := mi.String()

	magnet := &Magnet{
		InfoHash: btih,
		Name:     dn,
		Size:     0,
		Link:     finalLink,
	}
	return magnet, nil
}

func MagnetDisplayName(magnetLink string) string {
	mi, err := metainfo.ParseMagnetUri(magnetLink)
	if err != nil {
		return ""
	}
	return mi.DisplayName
}

func SetMagnetDisplayName(magnetLink, name string) string {
	name = strings.TrimSpace(name)
	if magnetLink == "" || name == "" {
		return magnetLink
	}
	parsed, err := url.Parse(magnetLink)
	if err != nil || parsed.Scheme != "magnet" {
		return magnetLink
	}
	encodedName := url.QueryEscape(name)
	base, query, ok := strings.Cut(magnetLink, "?")
	if !ok || strings.Contains(query, "?") {
		return magnetLink
	}
	queryParts := strings.Split(query, "&")
	for i, part := range queryParts {
		if strings.HasPrefix(part, "dn=") {
			queryParts[i] = "dn=" + encodedName
			return base + "?" + strings.Join(queryParts, "&")
		}
	}
	separator := "&"
	if query == "" {
		separator = ""
	}
	return magnetLink + separator + "dn=" + encodedName
}

// ExtractInfoHash returns the lowercase hex infohash of a magnet URI, or "".
func ExtractInfoHash(magnetDesc string) string {
	const prefix = "xt=urn:btih:"
	_, hash, ok := strings.Cut(magnetDesc, prefix)
	if !ok {
		return ""
	}
	if end := strings.IndexAny(hash, "&#"); end != -1 {
		hash = hash[:end]
	}
	hash, _ = processInfoHash(hash) // Convert to hex if needed
	return hash
}

func processInfoHash(input string) (string, error) {
	// Regular expression for a valid 40-character hex infohash

	// If it's already a valid hex infohash, return it as is
	if hexRegex.MatchString(input) {
		return strings.ToLower(input), nil
	}

	// If it's 32 characters long, it might be Base32 encoded
	if len(input) == base32InfoHashLen {
		// Ensure the input is uppercase and remove any padding
		input = strings.ToUpper(strings.TrimRight(input, "="))

		// Try to decode from Base32
		decoded, err := base32.StdEncoding.DecodeString(input)
		if err == nil && len(decoded) == 20 {
			// If successful and the result is 20 bytes, encode to hex
			return hex.EncodeToString(decoded), nil
		}
	}

	// If we get here, it's not a valid infohash and we couldn't convert it
	return "", fmt.Errorf("invalid infohash: %s", input)
}

func ConstructMagnet(infoHash, name string) *Magnet {
	// Only the link carries the escaped name; Name stays human-readable.
	name = strings.TrimSpace(name)
	return &Magnet{
		InfoHash: infoHash,
		Name:     name,
		Link:     fmt.Sprintf("magnet:?xt=urn:btih:%s&dn=%s", infoHash, url.QueryEscape(name)),
	}
}
