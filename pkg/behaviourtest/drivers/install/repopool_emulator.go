// repopool_emulator.go implements the RepoPoolEmulator driver: the
// external-mint pool, run against a GitHub emulator (ENVIRONMENT=emulator).
package install

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/fullsend-ai/fullsend/internal/e2etest"
	"github.com/fullsend-ai/fullsend/pkg/behaviourtest/drivers/install/common"
)

// emulatorOrgSetupTimeout bounds creating the pool orgs on the emulator.
const emulatorOrgSetupTimeout = time.Minute

// NewRepoPoolEmulator is a Factory for a pool on a GitHub emulator. It is
// the external-mint pool (mint at FULLSEND_MINT_URL, nothing deployed, each
// slot installed vendored, App set from BEHAVIOUR_APP_SET) with one
// addition: the emulator starts empty, so the pool's orgs are created
// first when they do not exist, owned by the token's user.
//
// Everything else is the live code path. Slot creation, Actions-readiness
// polling, slot GC and the rate-limit gate run unchanged; on a fresh
// emulator the GC finds nothing and the gate never sees a limit.
func NewRepoPoolEmulator(pool PoolConfig) (Driver, error) {
	if err := pool.validate(); err != nil {
		return nil, fmt.Errorf("emulator factory: %w", err)
	}
	mintURL := os.Getenv("FULLSEND_MINT_URL")
	if mintURL == "" {
		return nil, fmt.Errorf("emulator factory: FULLSEND_MINT_URL is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), emulatorOrgSetupTimeout)
	defer cancel()
	for _, org := range pool.Layout.Orgs() {
		token, err := pool.TokenFor(ctx, org)
		if err != nil {
			return nil, fmt.Errorf("emulator factory: token for %s: %w", org, err)
		}
		viewer := func(ctx context.Context) (string, error) { return e2etest.EmulatorViewerLogin(ctx, token) }
		if err := e2etest.EnsureEmulatorOrg(ctx, token, org, viewer); err != nil {
			return nil, fmt.Errorf("emulator factory: %w", err)
		}
	}
	newMint := func(mintGroup, orgCredential) (mintDriver, func(), error) {
		return &externalMintDriver{mintURL: mintURL}, func() {}, nil
	}
	// Setup names the App set the emulator was seeded with, as the
	// preview mints do, so bot logins match AppSet().
	opts := common.DefaultGitHubSetupOpts()
	opts.ConfigPreset = envConfigPreset()
	opts.AppSet = envAppSet()
	return buildPoolDriver(pool, nil, newMint, opts, traceStart{})
}

// Compile-time check: NewRepoPoolEmulator satisfies Factory.
var _ Factory = NewRepoPoolEmulator
