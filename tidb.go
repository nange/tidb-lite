// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package tidblite

import (
	"context"
	"database/sql"
	"fmt"
	"runtime"
	"strconv"
	"sync/atomic"

	_ "github.com/go-sql-driver/mysql"
	"github.com/pingcap/errors"
	"github.com/pingcap/log"
	"github.com/pingcap/parser/mysql"
	"github.com/pingcap/tidb/bindinfo"
	"github.com/pingcap/tidb/config"
	"github.com/pingcap/tidb/ddl"
	"github.com/pingcap/tidb/domain"
	"github.com/pingcap/tidb/kv"
	plannercore "github.com/pingcap/tidb/planner/core"
	"github.com/pingcap/tidb/privilege/privileges"
	"github.com/pingcap/tidb/server"
	"github.com/pingcap/tidb/session"
	"github.com/pingcap/tidb/sessionctx/variable"
	"github.com/pingcap/tidb/statistics"
	"github.com/pingcap/tidb/statistics/handle"
	kvstore "github.com/pingcap/tidb/store"
	"github.com/pingcap/tidb/store/mockstore"
	"github.com/pingcap/tidb/store/tikv"
	"github.com/pingcap/tidb/store/tikv/gcworker"
	"github.com/pingcap/tidb/util/logutil"
	"github.com/pingcap/tidb/util/memory"
	"github.com/pingcap/tidb/util/printer"
	"go.uber.org/zap"
)

// TiDBServer ...
type TiDBServer struct {
	cfg     *config.Config
	svr     *server.Server
	storage kv.Storage
	dom     *domain.Domain

	closeGracefully bool
}

// NewTiDBServer returns a new TiDBServer
func NewTiDBServer(options *Options) (*TiDBServer, error) {
	cfg := config.GetGlobalConfig()
	cfg.Store = "mocktikv"
	cfg.Path = options.DataDir
	cfg.Port = uint(options.Port)
	if err := cfg.Valid(); err != nil {
		return nil, errors.Annotatef(err, "invalid config")
	}

	tidbServer := &TiDBServer{
		cfg: cfg,
	}

	if err := tidbServer.registerStores(); err != nil {
		return nil, err
	}
	if err := tidbServer.setGlobalVars(); err != nil {
		return nil, err
	}
	if err := tidbServer.setupLog(); err != nil {
		return nil, err
	}
	tidbServer.printInfo()
	if err := tidbServer.createStoreAndDomain(); err != nil {
		return nil, err
	}
	if err := tidbServer.createServer(); err != nil {
		return nil, err
	}

	go func() {
		if err := tidbServer.runServer(); err != nil {
			log.Error("tidb lite run server failed", zap.Error(err))
		}
		tidbServer.cleanup(tidbServer.closeGracefully)
	}()

	return tidbServer, nil
}

func (t *TiDBServer) CreateConn() (*sql.DB, error) {
	dbDSN := fmt.Sprintf("%s:%s@tcp(%s:%d)/?charset=utf8mb4", "root", "", "127.0.0.1", t.cfg.Port)
	dbConn, err := sql.Open("mysql", dbDSN)
	if err != nil {
		return nil, errors.Trace(err)
	}

	return dbConn, nil
}

func (t *TiDBServer) Close() {
	t.serverShutdown(false)
}

func (t *TiDBServer) CloseGracefully() {
	t.serverShutdown(true)
}

func (t *TiDBServer) printInfo() {
	// Make sure the TiDB info is always printed.
	level := log.GetLevel()
	log.SetLevel(zap.InfoLevel)
	printer.PrintTiDBInfo()
	log.SetLevel(level)
}

func (t *TiDBServer) registerStores() error {
	if err := kvstore.Register("tikv", tikv.Driver{}); err != nil {
		return err
	}
	tikv.NewGCHandlerFunc = gcworker.NewGCWorker
	return kvstore.Register("mocktikv", mockstore.MockDriver{})
}

func (t *TiDBServer) createServer() error {
	driver := server.NewTiDBDriver(t.storage)
	var err error
	t.svr, err = server.NewServer(t.cfg, driver)
	if err != nil {
		// Both domain and storage have started, so we have to clean them before exiting.
		t.closeDomainAndStorage()
		return err
	}

	go t.dom.ExpensiveQueryHandle().SetSessionManager(t.svr).Run()
	return nil
}

func (t *TiDBServer) runServer() error {
	return t.svr.Run()
}

func (t *TiDBServer) createStoreAndDomain() error {
	fullPath := fmt.Sprintf("%s://%s", t.cfg.Store, t.cfg.Path)
	var err error
	t.storage, err = kvstore.New(fullPath)
	if err != nil {
		return err
	}
	// Bootstrap a session to load information schema.
	t.dom, err = session.BootstrapSession(t.storage)
	if err != nil {
		if err1 := t.storage.Close(); err1 != nil {
			log.Error("close tidb lite's storage failed", zap.Error(err1))
		}
		return err
	}
	return nil
}

func (t *TiDBServer) setGlobalVars() error {
	ddlLeaseDuration, err := parseDuration(t.cfg.Lease)
	if err != nil {
		return err
	}
	session.SetSchemaLease(ddlLeaseDuration)
	runtime.GOMAXPROCS(int(t.cfg.Performance.MaxProcs))
	statsLeaseDuration, err := parseDuration(t.cfg.Performance.StatsLease)
	if err != nil {
		return err
	}
	session.SetStatsLease(statsLeaseDuration)
	bindinfo.Lease, err = parseDuration(t.cfg.Performance.BindInfoLease)
	if err != nil {
		return err
	}
	domain.RunAutoAnalyze = t.cfg.Performance.RunAutoAnalyze
	statistics.FeedbackProbability.Store(t.cfg.Performance.FeedbackProbability)
	handle.MaxQueryFeedbackCount.Store(int64(t.cfg.Performance.QueryFeedbackLimit))
	statistics.RatioOfPseudoEstimate.Store(t.cfg.Performance.PseudoEstimateRatio)
	ddl.RunWorker = t.cfg.RunDDL
	if t.cfg.SplitTable {
		atomic.StoreUint32(&ddl.EnableSplitTableRegion, 1)
	}
	plannercore.AllowCartesianProduct.Store(t.cfg.Performance.CrossJoin)
	privileges.SkipWithGrant = t.cfg.Security.SkipGrantTable

	priority := mysql.Str2Priority(t.cfg.Performance.ForcePriority)
	variable.ForcePriority = int32(priority)
	variable.SysVars[variable.TiDBForcePriority].Value = mysql.Priority2Str[priority]

	variable.SysVars[variable.TIDBMemQuotaQuery].Value = strconv.FormatInt(t.cfg.MemQuotaQuery, 10)
	variable.SysVars["lower_case_table_names"].Value = strconv.Itoa(t.cfg.LowerCaseTableNames)
	variable.SysVars[variable.LogBin].Value = variable.BoolToIntStr(config.GetGlobalConfig().Binlog.Enable)

	variable.SysVars[variable.Port].Value = fmt.Sprintf("%d", t.cfg.Port)
	variable.SysVars[variable.Socket].Value = t.cfg.Socket
	variable.SysVars[variable.DataDir].Value = t.cfg.Path
	variable.SysVars[variable.TiDBSlowQueryFile].Value = t.cfg.Log.SlowQueryFile

	// For CI environment we default enable prepare-plan-cache.
	plannercore.SetPreparedPlanCache(config.CheckTableBeforeDrop || t.cfg.PreparedPlanCache.Enabled)
	if plannercore.PreparedPlanCacheEnabled() {
		plannercore.PreparedPlanCacheCapacity = t.cfg.PreparedPlanCache.Capacity
		plannercore.PreparedPlanCacheMemoryGuardRatio = t.cfg.PreparedPlanCache.MemoryGuardRatio
		if plannercore.PreparedPlanCacheMemoryGuardRatio < 0.0 || plannercore.PreparedPlanCacheMemoryGuardRatio > 1.0 {
			plannercore.PreparedPlanCacheMemoryGuardRatio = 0.1
		}
		plannercore.PreparedPlanCacheMaxMemory.Store(t.cfg.Performance.MaxMemory)
		total, err := memory.MemTotal()
		if err != nil {
			return err
		}
		if plannercore.PreparedPlanCacheMaxMemory.Load() > total || plannercore.PreparedPlanCacheMaxMemory.Load() <= 0 {
			plannercore.PreparedPlanCacheMaxMemory.Store(total)
		}
	}
	commitMaxBackoff, err := parseDuration(t.cfg.TiKVClient.CommitTimeout)
	if err != nil {
		return err
	}
	tikv.CommitMaxBackoff = int(commitMaxBackoff.Seconds() * 1000)

	pessimisticLockTTL, err := parseDuration(t.cfg.PessimisticTxn.TTL)
	if err != nil {
		return err
	}
	tikv.PessimisticLockTTL = uint64(pessimisticLockTTL.Seconds() * 1000)

	return nil
}

func (t *TiDBServer) serverShutdown(isgraceful bool) {
	t.closeGracefully = isgraceful
	t.svr.Close()
}

func (t *TiDBServer) closeDomainAndStorage() {
	atomic.StoreUint32(&tikv.ShuttingDown, 1)
	t.dom.Close()
	if err := t.storage.Close(); err != nil {
		log.Error("close tidb lite's storage failed", zap.Error(err))
	}
}

func (t *TiDBServer) cleanup(graceful bool) {
	if t.closeGracefully {
		t.svr.GracefulDown(context.Background(), nil)
	} else {
		t.svr.TryGracefulDown()
	}

	t.closeDomainAndStorage()
}

func (t *TiDBServer) setupLog() error {
	if err := logutil.InitZapLogger(t.cfg.Log.ToLogConfig()); err != nil {
		return err
	}

	if err := logutil.InitLogger(t.cfg.Log.ToLogConfig()); err != nil {
		return err
	}

	return nil
}
