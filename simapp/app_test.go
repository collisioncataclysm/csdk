package simapp

import (
	"encoding/json"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"
	abci "github.com/tendermint/tendermint/abci/types"
	"github.com/tendermint/tendermint/libs/log"
	tmproto "github.com/tendermint/tendermint/proto/tendermint/types"
	dbm "github.com/tendermint/tm-db"

	"github.com/cosmos/cosmos-sdk/baseapp"
	"github.com/cosmos/cosmos-sdk/tests/mocks"
	simtestutil "github.com/cosmos/cosmos-sdk/testutil/sims"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/module"
	"github.com/cosmos/cosmos-sdk/x/auth"
	"github.com/cosmos/cosmos-sdk/x/auth/vesting"
	authzmodule "github.com/cosmos/cosmos-sdk/x/authz/module"
	"github.com/cosmos/cosmos-sdk/x/bank"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/cosmos/cosmos-sdk/x/capability"
	"github.com/cosmos/cosmos-sdk/x/crisis"
	"github.com/cosmos/cosmos-sdk/x/distribution"
	"github.com/cosmos/cosmos-sdk/x/evidence"
	feegrantmodule "github.com/cosmos/cosmos-sdk/x/feegrant/module"
	"github.com/cosmos/cosmos-sdk/x/genutil"
	"github.com/cosmos/cosmos-sdk/x/gov"
	group "github.com/cosmos/cosmos-sdk/x/group/module"
	"github.com/cosmos/cosmos-sdk/x/mint"
	"github.com/cosmos/cosmos-sdk/x/params"
	"github.com/cosmos/cosmos-sdk/x/slashing"
	"github.com/cosmos/cosmos-sdk/x/staking"
	"github.com/cosmos/cosmos-sdk/x/upgrade"
)

func TestSimAppExportAndBlockedAddrs(t *testing.T) {
	encCfg := MakeTestEncodingConfig()
	db := dbm.NewMemDB()
	logger, _ := log.NewDefaultLogger("plain", "info", false)
	app := NewSimappWithCustomOptions(t, false, SetupOptions{
		Logger:         logger,
		DB:             db,
		InvCheckPeriod: 0,
		EncConfig:      encCfg,
		AppOpts:        simtestutil.NewAppOptionsWithFlagHome(DefaultNodeHome),
	})

	for acc := range maccPerms {
		require.True(
			t,
			app.BankKeeper.BlockedAddr(app.AccountKeeper.GetModuleAddress(acc)),
			"ensure that blocked addresses are properly set in bank keeper",
		)
	}

	app.Commit()

	logger2, _ := log.NewDefaultLogger("plain", "info", false)
	// Making a new app object with the db, so that initchain hasn't been called
	app2 := NewSimApp(logger2, db, nil, true, 0, encCfg, simtestutil.NewAppOptionsWithFlagHome(DefaultNodeHome))
	_, err := app2.ExportAppStateAndValidators(false, []string{})
	require.NoError(t, err, "ExportAppStateAndValidators should not have an error")
}

func TestGetMaccPerms(t *testing.T) {
	dup := GetMaccPerms()
	require.Equal(t, maccPerms, dup, "duplicated module account permissions differed from actual module account permissions")
}

func TestRunMigrations(t *testing.T) {
	db := dbm.NewMemDB()
	encCfg := MakeTestEncodingConfig()
	logger, _ := log.NewDefaultLogger("plain", "info", false)
	app := NewSimApp(logger, db, nil, true, 0, encCfg, simtestutil.NewAppOptionsWithFlagHome(DefaultNodeHome))

	// Create a new baseapp and configurator for the purpose of this test.
	bApp := baseapp.NewBaseApp(app.Name(), logger, db, encCfg.TxConfig.TxDecoder())
	bApp.SetCommitMultiStoreTracer(nil)
	bApp.SetInterfaceRegistry(encCfg.InterfaceRegistry)
	app.BaseApp = bApp
	configurator := module.NewConfigurator(app.appCodec, bApp.MsgServiceRouter(), app.GRPCQueryRouter())

	// We register all modules on the Configurator, except x/bank. x/bank will
	// serve as the test subject on which we run the migration tests.
	//
	// The loop below is the same as calling `RegisterServices` on
	// ModuleManager, except that we skip x/bank.
	for _, module := range app.ModuleManager.Modules {
		if module.Name() == banktypes.ModuleName {
			continue
		}

		module.RegisterServices(configurator)
	}

	// Initialize the chain
	app.InitChain(abci.RequestInitChain{})
	app.Commit()

	testCases := []struct {
		name         string
		moduleName   string
		fromVersion  uint64
		toVersion    uint64
		expRegErr    bool // errors while registering migration
		expRegErrMsg string
		expRunErr    bool // errors while running migration
		expRunErrMsg string
		expCalled    int
	}{
		{
			"cannot register migration for version 0",
			"bank", 0, 1,
			true, "module migration versions should start at 1: invalid version", false, "", 0,
		},
		{
			"throws error on RunMigrations if no migration registered for bank",
			"", 1, 2,
			false, "", true, "no migrations found for module bank: not found", 0,
		},
		{
			"can register 1->2 migration handler for x/bank, cannot run migration",
			"bank", 1, 2,
			false, "", true, "no migration found for module bank from version 2 to version 3: not found", 0,
		},
		{
			"can register 2->3 migration handler for x/bank, can run migration",
			"bank", 2, bank.AppModule{}.ConsensusVersion(),
			false, "", false, "", int(bank.AppModule{}.ConsensusVersion() - 2), // minus 2 because 1-2 is run in the previous test case.
		},
		{
			"cannot register migration handler for same module & fromVersion",
			"bank", 1, 2,
			true, "another migration for module bank and version 1 already exists: internal logic error", false, "", 0,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(tt *testing.T) {
			var err error

			// Since it's very hard to test actual in-place store migrations in
			// tests (due to the difficulty of maintaining multiple versions of a
			// module), we're just testing here that the migration logic is
			// called.
			called := 0

			if tc.moduleName != "" {
				for i := tc.fromVersion; i < tc.toVersion; i++ {
					// Register migration for module from version `fromVersion` to `fromVersion+1`.
					tt.Logf("Registering migration for %q v%d", tc.moduleName, i)
					err = configurator.RegisterMigration(tc.moduleName, i, func(sdk.Context) error {
						called++

						return nil
					})

					if tc.expRegErr {
						require.EqualError(tt, err, tc.expRegErrMsg)

						return
					}
					require.NoError(tt, err, "registering migration")
				}
			}

			// Run migrations only for bank. That's why we put the initial
			// version for bank as 1, and for all other modules, we put as
			// their latest ConsensusVersion.
			_, err = app.ModuleManager.RunMigrations(
				app.NewContext(true, tmproto.Header{Height: app.LastBlockHeight()}), configurator,
				module.VersionMap{
					"bank":         1,
					"auth":         auth.AppModule{}.ConsensusVersion(),
					"authz":        authzmodule.AppModule{}.ConsensusVersion(),
					"staking":      staking.AppModule{}.ConsensusVersion(),
					"mint":         mint.AppModule{}.ConsensusVersion(),
					"distribution": distribution.AppModule{}.ConsensusVersion(),
					"slashing":     slashing.AppModule{}.ConsensusVersion(),
					"gov":          gov.AppModule{}.ConsensusVersion(),
					"group":        group.AppModule{}.ConsensusVersion(),
					"params":       params.AppModule{}.ConsensusVersion(),
					"upgrade":      upgrade.AppModule{}.ConsensusVersion(),
					"vesting":      vesting.AppModule{}.ConsensusVersion(),
					"feegrant":     feegrantmodule.AppModule{}.ConsensusVersion(),
					"evidence":     evidence.AppModule{}.ConsensusVersion(),
					"crisis":       crisis.AppModule{}.ConsensusVersion(),
					"genutil":      genutil.AppModule{}.ConsensusVersion(),
					"capability":   capability.AppModule{}.ConsensusVersion(),
				},
			)
			if tc.expRunErr {
				require.EqualError(tt, err, tc.expRunErrMsg, "running migration")
			} else {
				require.NoError(tt, err, "running migration")
				// Make sure bank's migration is called.
				require.Equal(tt, tc.expCalled, called)
			}
		})
	}
}

func TestInitGenesisOnMigration(t *testing.T) {
	db := dbm.NewMemDB()
	encCfg := MakeTestEncodingConfig()
	logger, _ := log.NewDefaultLogger("plain", "info", false)
	app := NewSimApp(logger, db, nil, true, 0, encCfg, simtestutil.NewAppOptionsWithFlagHome(DefaultNodeHome))
	ctx := app.NewContext(true, tmproto.Header{Height: app.LastBlockHeight()})

	// Create a mock module. This module will serve as the new module we're
	// adding during a migration.
	mockCtrl := gomock.NewController(t)
	t.Cleanup(mockCtrl.Finish)
	mockModule := mocks.NewMockAppModule(mockCtrl)
	mockDefaultGenesis := json.RawMessage(`{"key": "value"}`)
	mockModule.EXPECT().DefaultGenesis(gomock.Eq(app.appCodec)).Times(1).Return(mockDefaultGenesis)
	mockModule.EXPECT().InitGenesis(gomock.Eq(ctx), gomock.Eq(app.appCodec), gomock.Eq(mockDefaultGenesis)).Times(1).Return(nil)
	mockModule.EXPECT().ConsensusVersion().Times(1).Return(uint64(0))

	app.ModuleManager.Modules["mock"] = mockModule

	// Run migrations only for "mock" module. We exclude it from
	// the VersionMap to simulate upgrading with a new module.
	_, err := app.ModuleManager.RunMigrations(ctx, app.Configurator(),
		module.VersionMap{
			"bank":         bank.AppModule{}.ConsensusVersion(),
			"auth":         auth.AppModule{}.ConsensusVersion(),
			"authz":        authzmodule.AppModule{}.ConsensusVersion(),
			"staking":      staking.AppModule{}.ConsensusVersion(),
			"mint":         mint.AppModule{}.ConsensusVersion(),
			"distribution": distribution.AppModule{}.ConsensusVersion(),
			"slashing":     slashing.AppModule{}.ConsensusVersion(),
			"gov":          gov.AppModule{}.ConsensusVersion(),
			"params":       params.AppModule{}.ConsensusVersion(),
			"upgrade":      upgrade.AppModule{}.ConsensusVersion(),
			"vesting":      vesting.AppModule{}.ConsensusVersion(),
			"feegrant":     feegrantmodule.AppModule{}.ConsensusVersion(),
			"evidence":     evidence.AppModule{}.ConsensusVersion(),
			"crisis":       crisis.AppModule{}.ConsensusVersion(),
			"genutil":      genutil.AppModule{}.ConsensusVersion(),
			"capability":   capability.AppModule{}.ConsensusVersion(),
		},
	)
	require.NoError(t, err)
}

func TestUpgradeStateOnGenesis(t *testing.T) {
	encCfg := MakeTestEncodingConfig()
	db := dbm.NewMemDB()
	logger, _ := log.NewDefaultLogger("plain", "info", false)
	app := NewSimappWithCustomOptions(t, false, SetupOptions{
		Logger:         logger,
		DB:             db,
		InvCheckPeriod: 0,
		EncConfig:      encCfg,
		AppOpts:        simtestutil.NewAppOptionsWithFlagHome(DefaultNodeHome),
	})

	// make sure the upgrade keeper has version map in state
	ctx := app.NewContext(false, tmproto.Header{})
	vm := app.UpgradeKeeper.GetModuleVersionMap(ctx)
	for v, i := range app.ModuleManager.Modules {
		require.Equal(t, vm[v], i.ConsensusVersion())
	}

	require.NotNil(t, app.UpgradeKeeper.GetVersionSetter())
}
