package utils_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sirrobot01/decypharr/internal/testutil"
	"github.com/sirrobot01/decypharr/internal/utils"
)

// ubuntuInfoHash is the infohash of the Ubuntu test torrent in testdata.
const (
	ubuntuInfoHash = "8a19577fb5f690970ca43a57ff1011ae202244b8"
	ubuntuName     = "ubuntu-25.04-desktop-amd64.iso"
)

// checkMagnet is a helper function that verifies magnet properties.
func checkMagnet(
	t *testing.T,
	magnet *utils.Magnet,
	expectedInfoHash, expectedName, expectedLink string,
	expectedTrackerCount int,
) {
	t.Helper() // This marks the function as a test helper

	// Verify basic properties
	if magnet.Name != expectedName {
		t.Errorf("Expected name '%s', got '%s'", expectedName, magnet.Name)
	}
	if magnet.InfoHash != expectedInfoHash {
		t.Errorf("Expected InfoHash '%s', got '%s'", expectedInfoHash, magnet.InfoHash)
	}
	if magnet.Link != expectedLink {
		t.Errorf("Expected Link '%s', got '%s'", expectedLink, magnet.Link)
	}

	// Verify the magnet link contains the essential info hash
	if !strings.Contains(magnet.Link, "xt=urn:btih:"+expectedInfoHash) {
		t.Error("Magnet link should contain info hash")
	}

	// Verify tracker count
	trCount := strings.Count(magnet.Link, "tr=")
	if trCount != expectedTrackerCount {
		t.Errorf("Expected %d tracker URLs, got %d", expectedTrackerCount, trCount)
	}
}

// testMagnetFromFile is a helper function for tests that use GetMagnetFromFile with file operations.
func testMagnetFromFile(
	t *testing.T,
	filePath string,
	rmTrackerUrls bool,
	expectedLink string,
	expectedTrackerCount int,
) {
	t.Helper()

	file, err := os.Open(filePath)
	if err != nil {
		t.Fatalf("Failed to open torrent file %s: %v", filePath, err)
	}
	defer file.Close()

	magnet, err := utils.GetMagnetFromFile(file, filepath.Base(filePath), rmTrackerUrls)
	if err != nil {
		t.Fatalf("GetMagnetFromFile failed: %v", err)
	}

	checkMagnet(t, magnet, ubuntuInfoHash, ubuntuName, expectedLink, expectedTrackerCount)

	// Log the result
	if rmTrackerUrls {
		t.Logf("Generated clean magnet link: %s", magnet.Link)
	} else {
		t.Logf("Generated magnet link with trackers: %s", magnet.Link)
	}
}

func TestGetMagnetFromFile_RealTorrentFile_StripTrue(t *testing.T) {
	t.Parallel()
	expectedLink := "magnet:?xt=urn:btih:8a19577fb5f690970ca43a57ff1011ae202244b8&dn=ubuntu-25.04-desktop-amd64.iso"
	expectedTrackerCount := 0 // Should be 0 when stripping trackers

	torrentPath := testutil.GetTestTorrentPath()
	testMagnetFromFile(t, torrentPath, true, expectedLink, expectedTrackerCount)
}

func TestGetMagnetFromFile_RealTorrentFile_StripFalse(t *testing.T) {
	t.Parallel()
	expectedLink := "magnet:?xt=urn:btih:8a19577fb5f690970ca43a57ff1011ae202244b8&dn=ubuntu-25.04-desktop-amd64.iso&tr=https%3A%2F%2Ftorrent.ubuntu.com%2Fannounce&tr=https%3A%2F%2Fipv6.torrent.ubuntu.com%2Fannounce"
	expectedTrackerCount := 2 // Should be 2 when preserving trackers

	torrentPath := testutil.GetTestTorrentPath()
	testMagnetFromFile(t, torrentPath, false, expectedLink, expectedTrackerCount)
}

func TestGetMagnetFromFile_UsesUploadedFilenameAsDisplayName(t *testing.T) {
	t.Parallel()
	file, err := os.Open(testutil.GetTestTorrentPath())
	if err != nil {
		t.Fatalf("Failed to open torrent file: %v", err)
	}
	defer file.Close()

	magnet, err := utils.GetMagnetFromFile(file, "Example Show Season 01 S01 1080p WEB-DL x265.torrent", true)
	if err != nil {
		t.Fatalf("GetMagnetFromFile failed: %v", err)
	}

	want := "Example Show Season 01 S01 1080p WEB-DL x265"
	if magnet.Name != want {
		t.Fatalf("expected name %q, got %q", want, magnet.Name)
	}
	if got := utils.MagnetDisplayName(magnet.Link); got != want {
		t.Fatalf("expected display name %q, got %q", want, got)
	}
}

func TestGetMagnetFromFile_StripsUploadedTorrentPathFromDisplayName(t *testing.T) {
	t.Parallel()
	file, err := os.Open(testutil.GetTestTorrentPath())
	if err != nil {
		t.Fatalf("Failed to open torrent file: %v", err)
	}
	defer file.Close()

	magnet, err := utils.GetMagnetFromFile(file, "/tmp/Example Show Season 01 S01 1080p WEB-DL x265.torrent", true)
	if err != nil {
		t.Fatalf("GetMagnetFromFile failed: %v", err)
	}

	want := "Example Show Season 01 S01 1080p WEB-DL x265"
	if magnet.Name != want {
		t.Fatalf("expected name %q, got %q", want, magnet.Name)
	}
	if got := utils.MagnetDisplayName(magnet.Link); got != want {
		t.Fatalf("expected display name %q, got %q", want, got)
	}
}

func TestGetMagnetFromFile_MagnetFileKeepsEmbeddedDisplayName(t *testing.T) {
	t.Parallel()
	file := strings.NewReader("magnet:?xt=urn:btih:8a19577fb5f690970ca43a57ff1011ae202244b8&dn=Embedded+Release+Name")

	magnet, err := utils.GetMagnetFromFile(file, "uploaded-name.magnet", true)
	if err != nil {
		t.Fatalf("GetMagnetFromFile failed: %v", err)
	}

	if got, want := magnet.Name, "Embedded Release Name"; got != want {
		t.Fatalf("expected name %q, got %q", want, got)
	}
	if got, want := utils.MagnetDisplayName(magnet.Link), "Embedded Release Name"; got != want {
		t.Fatalf("expected display name %q, got %q", want, got)
	}
}

func TestGetMagnetFromFile_MagnetFileWithoutDisplayNameUsesUploadedFilename(t *testing.T) {
	t.Parallel()
	file := strings.NewReader("magnet:?xt=urn:btih:8a19577fb5f690970ca43a57ff1011ae202244b8")

	magnet, err := utils.GetMagnetFromFile(file, "uploaded-release-name.magnet", true)
	if err != nil {
		t.Fatalf("GetMagnetFromFile failed: %v", err)
	}

	if got, want := magnet.Name, "uploaded-release-name"; got != want {
		t.Fatalf("expected name %q, got %q", want, got)
	}
	if got, want := utils.MagnetDisplayName(magnet.Link), "uploaded-release-name"; got != want {
		t.Fatalf("expected display name %q, got %q", want, got)
	}
}

func TestGetMagnetFromFile_MagnetFile_StripTrue(t *testing.T) {
	t.Parallel()
	expectedLink := "magnet:?xt=urn:btih:8a19577fb5f690970ca43a57ff1011ae202244b8&dn=ubuntu-25.04-desktop-amd64.iso"
	expectedTrackerCount := 0 // Should be 0 when stripping trackers

	torrentPath := testutil.GetTestMagnetPath()
	testMagnetFromFile(t, torrentPath, true, expectedLink, expectedTrackerCount)
}

func TestGetMagnetFromFile_MagnetFile_StripFalse(t *testing.T) {
	t.Parallel()
	expectedLink := "magnet:?xt=urn:btih:8a19577fb5f690970ca43a57ff1011ae202244b8&dn=ubuntu-25.04-desktop-amd64.iso&tr=https%3A%2F%2Fipv6.torrent.ubuntu.com%2Fannounce&tr=https%3A%2F%2Ftorrent.ubuntu.com%2Fannounce"
	expectedTrackerCount := 2

	torrentPath := testutil.GetTestMagnetPath()
	testMagnetFromFile(t, torrentPath, false, expectedLink, expectedTrackerCount)
}

func TestGetMagnetFromURL_MagnetLink_StripTrue(t *testing.T) {
	t.Parallel()
	expectedInfoHash := ubuntuInfoHash
	expectedName := "ubuntu-25.04-desktop-amd64.iso"
	expectedLink := "magnet:?xt=urn:btih:8a19577fb5f690970ca43a57ff1011ae202244b8&dn=ubuntu-25.04-desktop-amd64.iso"
	expectedTrackerCount := 0

	// Load the magnet URL from the test file
	magnetURL, err := testutil.GetTestMagnetContent()
	if err != nil {
		t.Fatalf("Failed to load magnet URL from test file: %v", err)
	}

	magnet, err := utils.GetMagnetFromURL(magnetURL, true)
	if err != nil {
		t.Fatalf("GetMagnetFromURL failed: %v", err)
	}

	checkMagnet(t, magnet, expectedInfoHash, expectedName, expectedLink, expectedTrackerCount)
	t.Logf("Generated clean magnet link: %s", magnet.Link)
}

func TestGetMagnetFromURL_MagnetLink_StripFalse(t *testing.T) {
	t.Parallel()
	expectedInfoHash := ubuntuInfoHash
	expectedName := "ubuntu-25.04-desktop-amd64.iso"
	expectedLink := "magnet:?xt=urn:btih:8a19577fb5f690970ca43a57ff1011ae202244b8&dn=ubuntu-25.04-desktop-amd64.iso&tr=https%3A%2F%2Fipv6.torrent.ubuntu.com%2Fannounce&tr=https%3A%2F%2Ftorrent.ubuntu.com%2Fannounce"
	expectedTrackerCount := 2

	// Load the magnet URL from the test file
	magnetURL, err := testutil.GetTestMagnetContent()
	if err != nil {
		t.Fatalf("Failed to load magnet URL from test file: %v", err)
	}

	magnet, err := utils.GetMagnetFromURL(magnetURL, false)
	if err != nil {
		t.Fatalf("GetMagnetFromURL failed: %v", err)
	}

	checkMagnet(t, magnet, expectedInfoHash, expectedName, expectedLink, expectedTrackerCount)
	t.Logf("Generated magnet link with trackers: %s", magnet.Link)
}

// testMagnetFromHTTPTorrent is a helper function for tests that use GetMagnetFromURL with HTTP torrent links.
func testMagnetFromHTTPTorrent(
	t *testing.T,
	torrentPath string,
	rmTrackerUrls bool,
	expectedInfoHash, expectedName, expectedLink string,
	expectedTrackerCount int,
) {
	t.Helper()

	// Read the torrent file content
	torrentData, err := testutil.GetTestDataBytes(torrentPath)
	if err != nil {
		t.Fatalf("Failed to read torrent file: %v", err)
	}

	// Create a test HTTP server that serves the torrent file
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-bittorrent")
		_, _ = w.Write(torrentData)
	}))
	defer server.Close()

	// Test the function with the mock server URL
	magnet, err := utils.GetMagnetFromURL(server.URL, rmTrackerUrls)
	if err != nil {
		t.Fatalf("GetMagnetFromURL failed: %v", err)
	}

	checkMagnet(t, magnet, expectedInfoHash, expectedName, expectedLink, expectedTrackerCount)

	// Log the result
	if rmTrackerUrls {
		t.Logf("Generated clean magnet link from HTTP torrent: %s", magnet.Link)
	} else {
		t.Logf("Generated magnet link with trackers from HTTP torrent: %s", magnet.Link)
	}
}

func TestGetMagnetFromURL_TorrentLink_StripTrue(t *testing.T) {
	t.Parallel()
	expectedInfoHash := ubuntuInfoHash
	expectedName := "ubuntu-25.04-desktop-amd64.iso"
	expectedLink := "magnet:?xt=urn:btih:8a19577fb5f690970ca43a57ff1011ae202244b8&dn=ubuntu-25.04-desktop-amd64.iso"
	expectedTrackerCount := 0

	testMagnetFromHTTPTorrent(
		t,
		"ubuntu-25.04-desktop-amd64.iso.torrent",
		true,
		expectedInfoHash,
		expectedName,
		expectedLink,
		expectedTrackerCount,
	)
}

func TestGetMagnetFromURL_TorrentLink_StripFalse(t *testing.T) {
	t.Parallel()
	expectedInfoHash := ubuntuInfoHash
	expectedName := "ubuntu-25.04-desktop-amd64.iso"
	expectedLink := "magnet:?xt=urn:btih:8a19577fb5f690970ca43a57ff1011ae202244b8&dn=ubuntu-25.04-desktop-amd64.iso&tr=https%3A%2F%2Ftorrent.ubuntu.com%2Fannounce&tr=https%3A%2F%2Fipv6.torrent.ubuntu.com%2Fannounce"
	expectedTrackerCount := 2

	testMagnetFromHTTPTorrent(
		t,
		"ubuntu-25.04-desktop-amd64.iso.torrent",
		false,
		expectedInfoHash,
		expectedName,
		expectedLink,
		expectedTrackerCount,
	)
}
