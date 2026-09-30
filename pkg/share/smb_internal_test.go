package share

import (
	"bytes"
	"context"
	"testing"

	"github.com/sirrobot01/facetfs/smb"

	"github.com/sirrobot01/decypharr/internal/config"
)

func TestSingleUserAuthenticator(t *testing.T) {
	t.Parallel()
	auth := &singleUser{user: "media", hash: smb.NTHash("secret")}
	ctx := context.Background()

	hash, err := auth.NTHash(ctx, "SOME-PC", "media")
	if err != nil {
		t.Fatalf("known user rejected: %v", err)
	}
	if !bytes.Equal(hash, smb.NTHash("secret")) {
		t.Fatal("wrong hash returned")
	}

	// Case-insensitive user, arbitrary domain: Windows sends its machine
	// name as the domain.
	if _, ntHashErr := auth.NTHash(ctx, "WORKGROUP", "MEDIA"); ntHashErr != nil {
		t.Fatalf("case-insensitive match failed: %v", ntHashErr)
	}

	if _, ntHashErr := auth.NTHash(ctx, "WORKGROUP", "intruder"); ntHashErr == nil {
		t.Fatal("unknown user was accepted")
	}
}

func TestSMBRequiresCredentials(t *testing.T) { //nolint:paralleltest // mutates the config singleton
	mgr := testManager(t)
	server := NewSMB(mgr, nil, config.SMB{Enabled: true, Username: "media"})
	if err := server.Start(context.Background()); err == nil {
		t.Fatal("expected an error without a password")
	}
}
