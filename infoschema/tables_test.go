// Copyright 2018 PingCAP, Inc.
//
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

package infoschema_test

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	. "github.com/pingcap/check"
	"github.com/pingcap/failpoint"
	"github.com/pingcap/fn"
	"github.com/pingcap/parser/auth"
	"github.com/pingcap/parser/model"
	"github.com/pingcap/parser/mysql"
	"github.com/pingcap/parser/terror"
	"github.com/pingcap/tidb/config"
	"github.com/pingcap/tidb/domain"
	"github.com/pingcap/tidb/domain/infosync"
	"github.com/pingcap/tidb/infoschema"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/meta/autoid"
	"github.com/pingcap/tidb/server"
	"github.com/pingcap/tidb/session"
	"github.com/pingcap/tidb/statistics"
	"github.com/pingcap/tidb/statistics/handle"
	"github.com/pingcap/tidb/store/helper"
	"github.com/pingcap/tidb/store/mockstore"
	"github.com/pingcap/tidb/store/tikv"
	"github.com/pingcap/tidb/util"
	"github.com/pingcap/tidb/util/pdapi"
	"github.com/pingcap/tidb/util/testkit"
	"github.com/pingcap/tidb/util/testleak"
	"github.com/pingcap/tidb/util/testutil"
	"google.golang.org/grpc"
)

var _ = Suite(&testTableSuite{})
var _ = SerialSuites(&testClusterTableSuite{testTableSuite: &testTableSuite{}})

type testTableSuite struct {
	store kv.Storage
	dom   *domain.Domain
}

func (s *testTableSuite) SetUpSuite(c *C) {
	testleak.BeforeTest()

	var err error
	s.store, err = mockstore.NewMockTikvStore()
	c.Assert(err, IsNil)
	session.DisableStats4Test()
	s.dom, err = session.BootstrapSession(s.store)
	c.Assert(err, IsNil)
}

func (s *testTableSuite) TearDownSuite(c *C) {
	s.dom.Close()
	s.store.Close()
	testleak.AfterTest(c)()
}

type testClusterTableSuite struct {
	*testTableSuite
	rpcserver  *grpc.Server
	httpServer *httptest.Server
	mockAddr   string
}

func (s *testClusterTableSuite) SetUpSuite(c *C) {
	s.testTableSuite.SetUpSuite(c)
	s.rpcserver = s.setUpRPCService(c, "0.0.0.0:10080")
	s.httpServer, s.mockAddr = setUpMockPDHTTPSercer()
}

func (s *testClusterTableSuite) setUpRPCService(c *C, addr string) *grpc.Server {
	lis, err := net.Listen("tcp", addr)
	c.Assert(err, IsNil)
	// Fix issue 9836
	sm := &mockSessionManager{make(map[uint64]*util.ProcessInfo, 1)}
	sm.processInfoMap[1] = &util.ProcessInfo{
		ID:      1,
		User:    "root",
		Host:    "127.0.0.1",
		Command: mysql.ComQuery,
	}
	srv := server.NewRPCServer(config.GetGlobalConfig().Security, s.dom, sm)
	go func() {
		err = srv.Serve(lis)
		c.Assert(err, IsNil)
	}()
	return srv
}

func setUpMockPDHTTPSercer() (*httptest.Server, string) {
	// mock PD http server
	router := mux.NewRouter()
	server := httptest.NewServer(router)
	// mock store stats stat
	mockAddr := strings.TrimPrefix(server.URL, "http://")
	router.Handle(pdapi.Stores, fn.Wrap(func() (*helper.StoresStat, error) {
		return &helper.StoresStat{
			Count: 1,
			Stores: []helper.StoreStat{
				{
					Store: helper.StoreBaseStat{
						ID:            1,
						Address:       "127.0.0.1:20160",
						State:         0,
						StateName:     "Up",
						Version:       "4.0.0-alpha",
						StatusAddress: mockAddr,
						GitHash:       "mock-tikv-githash",
					},
				},
			},
		}, nil
	}))
	// mock PD API
	router.Handle(pdapi.ClusterVersion, fn.Wrap(func() (string, error) { return "4.0.0-alpha", nil }))
	router.Handle(pdapi.Status, fn.Wrap(func() (interface{}, error) {
		return struct {
			GitHash string `json:"git_hash"`
		}{GitHash: "mock-pd-githash"}, nil
	}))
	var mockConfig = func() (map[string]interface{}, error) {
		configuration := map[string]interface{}{
			"key1": "value1",
			"key2": map[string]string{
				"nest1": "n-value1",
				"nest2": "n-value2",
			},
			"key3": map[string]interface{}{
				"nest1": "n-value1",
				"nest2": "n-value2",
				"key4": map[string]string{
					"nest3": "n-value4",
					"nest4": "n-value5",
				},
			},
		}
		return configuration, nil
	}
	// pd config
	router.Handle(pdapi.Config, fn.Wrap(mockConfig))
	// TiDB/TiKV config
	router.Handle("/config", fn.Wrap(mockConfig))
	return server, mockAddr
}

func (s *testClusterTableSuite) TearDownSuite(c *C) {
	if s.rpcserver != nil {
		s.rpcserver.Stop()
		s.rpcserver = nil
	}
	if s.httpServer != nil {
		s.httpServer.Close()
	}
	s.testTableSuite.TearDownSuite(c)
}

func (s *testTableSuite) TestInfoschemaFieldValue(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists numschema, timeschema")
	tk.MustExec("create table numschema(i int(2), f float(4,2), d decimal(4,3))")
	tk.MustExec("create table timeschema(d date, dt datetime(3), ts timestamp(3), t time(4), y year(4))")
	tk.MustExec("create table strschema(c char(3), c2 varchar(3), b blob(3), t text(3))")
	tk.MustExec("create table floatschema(a float, b double(7, 3))")

	tk.MustQuery("select CHARACTER_MAXIMUM_LENGTH,CHARACTER_OCTET_LENGTH,NUMERIC_PRECISION,NUMERIC_SCALE,DATETIME_PRECISION from information_schema.COLUMNS where table_name='numschema'").
		Check(testkit.Rows("<nil> <nil> 2 0 <nil>", "<nil> <nil> 4 2 <nil>", "<nil> <nil> 4 3 <nil>")) // FIXME: for mysql first one will be "<nil> <nil> 10 0 <nil>"
	tk.MustQuery("select CHARACTER_MAXIMUM_LENGTH,CHARACTER_OCTET_LENGTH,NUMERIC_PRECISION,NUMERIC_SCALE,DATETIME_PRECISION from information_schema.COLUMNS where table_name='timeschema'").
		Check(testkit.Rows("<nil> <nil> <nil> <nil> <nil>", "<nil> <nil> <nil> <nil> 3", "<nil> <nil> <nil> <nil> 3", "<nil> <nil> <nil> <nil> 4", "<nil> <nil> <nil> <nil> <nil>"))
	tk.MustQuery("select CHARACTER_MAXIMUM_LENGTH,CHARACTER_OCTET_LENGTH,NUMERIC_PRECISION,NUMERIC_SCALE,DATETIME_PRECISION from information_schema.COLUMNS where table_name='strschema'").
		Check(testkit.Rows("3 3 <nil> <nil> <nil>", "3 3 <nil> <nil> <nil>", "3 3 <nil> <nil> <nil>", "3 3 <nil> <nil> <nil>")) // FIXME: for mysql last two will be "255 255 <nil> <nil> <nil>", "255 255 <nil> <nil> <nil>"
	tk.MustQuery("select NUMERIC_SCALE from information_schema.COLUMNS where table_name='floatschema'").
		Check(testkit.Rows("<nil>", "3"))

	// Test for auto increment ID.
	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t (c int auto_increment primary key, d int)")
	tk.MustQuery("select auto_increment from information_schema.tables where table_name='t'").Check(
		testkit.Rows("1"))
	tk.MustExec("insert into t(c, d) values(1, 1)")
	tk.MustQuery("select auto_increment from information_schema.tables where table_name='t'").Check(
		testkit.Rows("2"))

	tk.MustQuery("show create table t").Check(
		testkit.Rows("" +
			"t CREATE TABLE `t` (\n" +
			"  `c` int(11) NOT NULL AUTO_INCREMENT,\n" +
			"  `d` int(11) DEFAULT NULL,\n" +
			"  PRIMARY KEY (`c`)\n" +
			") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin AUTO_INCREMENT=30002"))

	// Test auto_increment for table without auto_increment column
	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t (d int)")
	tk.MustQuery("select auto_increment from information_schema.tables where table_name='t'").Check(
		testkit.Rows("<nil>"))

	tk.MustExec("create user xxx")
	tk.MustExec("flush privileges")

	// Test for length of enum and set
	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t ( s set('a','bc','def','ghij') default NULL, e1 enum('a', 'ab', 'cdef'), s2 SET('1','2','3','4','1585','ONE','TWO','Y','N','THREE'))")
	tk.MustQuery("select column_name, character_maximum_length from information_schema.columns where table_schema=Database() and table_name = 't' and column_name = 's'").Check(
		testkit.Rows("s 13"))
	tk.MustQuery("select column_name, character_maximum_length from information_schema.columns where table_schema=Database() and table_name = 't' and column_name = 's2'").Check(
		testkit.Rows("s2 30"))
	tk.MustQuery("select column_name, character_maximum_length from information_schema.columns where table_schema=Database() and table_name = 't' and column_name = 'e1'").Check(
		testkit.Rows("e1 4"))

	tk1 := testkit.NewTestKit(c, s.store)
	tk1.MustExec("use test")
	c.Assert(tk1.Se.Auth(&auth.UserIdentity{
		Username: "xxx",
		Hostname: "127.0.0.1",
	}, nil, nil), IsTrue)

	tk1.MustQuery("select distinct(table_schema) from information_schema.tables").Check(testkit.Rows("INFORMATION_SCHEMA"))

	// Fix issue 9836
	sm := &mockSessionManager{make(map[uint64]*util.ProcessInfo, 1)}
	sm.processInfoMap[1] = &util.ProcessInfo{
		ID:      1,
		User:    "root",
		Host:    "127.0.0.1",
		Command: mysql.ComQuery,
		StmtCtx: tk.Se.GetSessionVars().StmtCtx,
	}
	tk.Se.SetSessionManager(sm)
	tk.MustQuery("SELECT user,host,command FROM information_schema.processlist;").Check(testkit.Rows("root 127.0.0.1 Query"))
}

func (s *testTableSuite) TestDataForTableStatsField(c *C) {
	s.dom.SetStatsUpdating(true)
	oldExpiryTime := infoschema.TableStatsCacheExpiry
	infoschema.TableStatsCacheExpiry = 0
	defer func() { infoschema.TableStatsCacheExpiry = oldExpiryTime }()

	do := s.dom
	h := do.StatsHandle()
	h.Clear()
	is := do.InfoSchema()
	tk := testkit.NewTestKit(c, s.store)

	tk.MustExec("use test")
	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t (c int, d int, e char(5), index idx(e))")
	h.HandleDDLEvent(<-h.DDLEventCh())
	tk.MustQuery("select table_rows, avg_row_length, data_length, index_length from information_schema.tables where table_name='t'").Check(
		testkit.Rows("0 0 0 0"))
	tk.MustExec(`insert into t(c, d, e) values(1, 2, "c"), (2, 3, "d"), (3, 4, "e")`)
	c.Assert(h.DumpStatsDeltaToKV(handle.DumpAll), IsNil)
	c.Assert(h.Update(is), IsNil)
	tk.MustQuery("select table_rows, avg_row_length, data_length, index_length from information_schema.tables where table_name='t'").Check(
		testkit.Rows("3 18 54 6"))
	tk.MustExec(`insert into t(c, d, e) values(4, 5, "f")`)
	c.Assert(h.DumpStatsDeltaToKV(handle.DumpAll), IsNil)
	c.Assert(h.Update(is), IsNil)
	tk.MustQuery("select table_rows, avg_row_length, data_length, index_length from information_schema.tables where table_name='t'").Check(
		testkit.Rows("4 18 72 8"))
	tk.MustExec("delete from t where c >= 3")
	c.Assert(h.DumpStatsDeltaToKV(handle.DumpAll), IsNil)
	c.Assert(h.Update(is), IsNil)
	tk.MustQuery("select table_rows, avg_row_length, data_length, index_length from information_schema.tables where table_name='t'").Check(
		testkit.Rows("2 18 36 4"))
	tk.MustExec("delete from t where c=3")
	c.Assert(h.DumpStatsDeltaToKV(handle.DumpAll), IsNil)
	c.Assert(h.Update(is), IsNil)
	tk.MustQuery("select table_rows, avg_row_length, data_length, index_length from information_schema.tables where table_name='t'").Check(
		testkit.Rows("2 18 36 4"))

	// Test partition table.
	tk.MustExec("drop table if exists t")
	tk.MustExec(`CREATE TABLE t (a int, b int, c varchar(5), primary key(a), index idx(c)) PARTITION BY RANGE (a) (PARTITION p0 VALUES LESS THAN (6), PARTITION p1 VALUES LESS THAN (11), PARTITION p2 VALUES LESS THAN (16))`)
	h.HandleDDLEvent(<-h.DDLEventCh())
	tk.MustExec(`insert into t(a, b, c) values(1, 2, "c"), (7, 3, "d"), (12, 4, "e")`)
	c.Assert(h.DumpStatsDeltaToKV(handle.DumpAll), IsNil)
	c.Assert(h.Update(is), IsNil)
	tk.MustQuery("select table_rows, avg_row_length, data_length, index_length from information_schema.tables where table_name='t'").Check(
		testkit.Rows("3 18 54 6"))
}

func (s *testTableSuite) TestCharacterSetCollations(c *C) {
	tk := testkit.NewTestKit(c, s.store)

	// The description column is not important
	tk.MustQuery("SELECT default_collate_name, maxlen FROM information_schema.character_sets ORDER BY character_set_name").Check(
		testkit.Rows("ascii_bin 1", "binary 1", "latin1_bin 1", "utf8_bin 3", "utf8mb4_bin 4"))

	// The is_default column is not important
	// but the id's are used by client libraries and must be stable
	tk.MustQuery("SELECT character_set_name, id, sortlen FROM information_schema.collations ORDER BY collation_name").Check(
		testkit.Rows("ascii 65 1", "binary 63 1", "latin1 47 1", "utf8 83 1", "utf8mb4 46 1"))

	// Test charset/collation in information_schema.COLUMNS table.
	tk.MustExec("DROP DATABASE IF EXISTS charset_collate_test")
	tk.MustExec("CREATE DATABASE charset_collate_test; USE charset_collate_test")

	// TODO: Specifying the charset for national char/varchar should not be supported.
	tk.MustExec(`CREATE TABLE charset_collate_col_test(
		c_int int,
		c_float float,
		c_bit bit,
		c_bool bool,
		c_char char(1) charset ascii collate ascii_bin,
		c_nchar national char(1) charset ascii collate ascii_bin,
		c_binary binary,
		c_varchar varchar(1) charset ascii collate ascii_bin,
		c_nvarchar national varchar(1) charset ascii collate ascii_bin,
		c_varbinary varbinary(1),
		c_year year,
		c_date date,
		c_time time,
		c_datetime datetime,
		c_timestamp timestamp,
		c_blob blob,
		c_tinyblob tinyblob,
		c_mediumblob mediumblob,
		c_longblob longblob,
		c_text text charset ascii collate ascii_bin,
		c_tinytext tinytext charset ascii collate ascii_bin,
		c_mediumtext mediumtext charset ascii collate ascii_bin,
		c_longtext longtext charset ascii collate ascii_bin,
		c_json json,
		c_enum enum('1') charset ascii collate ascii_bin,
		c_set set('1') charset ascii collate ascii_bin
	)`)

	tk.MustQuery(`SELECT column_name, character_set_name, collation_name
					FROM information_schema.COLUMNS
					WHERE table_schema = "charset_collate_test" AND table_name = "charset_collate_col_test"
					ORDER BY column_name`,
	).Check(testkit.Rows(
		"c_binary <nil> <nil>",
		"c_bit <nil> <nil>",
		"c_blob <nil> <nil>",
		"c_bool <nil> <nil>",
		"c_char ascii ascii_bin",
		"c_date <nil> <nil>",
		"c_datetime <nil> <nil>",
		"c_enum ascii ascii_bin",
		"c_float <nil> <nil>",
		"c_int <nil> <nil>",
		"c_json <nil> <nil>",
		"c_longblob <nil> <nil>",
		"c_longtext ascii ascii_bin",
		"c_mediumblob <nil> <nil>",
		"c_mediumtext ascii ascii_bin",
		"c_nchar ascii ascii_bin",
		"c_nvarchar ascii ascii_bin",
		"c_set ascii ascii_bin",
		"c_text ascii ascii_bin",
		"c_time <nil> <nil>",
		"c_timestamp <nil> <nil>",
		"c_tinyblob <nil> <nil>",
		"c_tinytext ascii ascii_bin",
		"c_varbinary <nil> <nil>",
		"c_varchar ascii ascii_bin",
		"c_year <nil> <nil>",
	))
	tk.MustExec("DROP DATABASE charset_collate_test")
}

func (s *testTableSuite) TestCurrentTimestampAsDefault(c *C) {
	tk := testkit.NewTestKit(c, s.store)

	tk.MustExec("DROP DATABASE IF EXISTS default_time_test")
	tk.MustExec("CREATE DATABASE default_time_test; USE default_time_test")

	tk.MustExec(`CREATE TABLE default_time_table(
					c_datetime datetime,
					c_datetime_default datetime default current_timestamp,
					c_datetime_default_2 datetime(2) default current_timestamp(2),
					c_timestamp timestamp,
					c_timestamp_default timestamp default current_timestamp,
					c_timestamp_default_3 timestamp(3) default current_timestamp(3),
					c_varchar_default varchar(20) default "current_timestamp",
					c_varchar_default_3 varchar(20) default "current_timestamp(3)",
					c_varchar_default_on_update datetime default current_timestamp on update current_timestamp,
					c_varchar_default_on_update_fsp datetime(3) default current_timestamp(3) on update current_timestamp(3),
					c_varchar_default_with_case varchar(20) default "cUrrent_tImestamp"
				);`)

	tk.MustQuery(`SELECT column_name, column_default, extra
					FROM information_schema.COLUMNS
					WHERE table_schema = "default_time_test" AND table_name = "default_time_table"
					ORDER BY column_name`,
	).Check(testkit.Rows(
		"c_datetime <nil> ",
		"c_datetime_default CURRENT_TIMESTAMP ",
		"c_datetime_default_2 CURRENT_TIMESTAMP(2) ",
		"c_timestamp <nil> ",
		"c_timestamp_default CURRENT_TIMESTAMP ",
		"c_timestamp_default_3 CURRENT_TIMESTAMP(3) ",
		"c_varchar_default current_timestamp ",
		"c_varchar_default_3 current_timestamp(3) ",
		"c_varchar_default_on_update CURRENT_TIMESTAMP DEFAULT_GENERATED on update CURRENT_TIMESTAMP",
		"c_varchar_default_on_update_fsp CURRENT_TIMESTAMP(3) DEFAULT_GENERATED on update CURRENT_TIMESTAMP(3)",
		"c_varchar_default_with_case cUrrent_tImestamp ",
	))
	tk.MustExec("DROP DATABASE default_time_test")
}

type mockSessionManager struct {
	processInfoMap map[uint64]*util.ProcessInfo
}

func (sm *mockSessionManager) ShowProcessList() map[uint64]*util.ProcessInfo { return sm.processInfoMap }

func (sm *mockSessionManager) GetProcessInfo(id uint64) (*util.ProcessInfo, bool) {
	rs, ok := sm.processInfoMap[id]
	return rs, ok
}

func (sm *mockSessionManager) Kill(connectionID uint64, query bool) {}

func (s *testTableSuite) TestSomeTables(c *C) {
	tk := testkit.NewTestKit(c, s.store)

	tk.MustQuery("select * from information_schema.COLLATION_CHARACTER_SET_APPLICABILITY where COLLATION_NAME='utf8mb4_bin';").Check(
		testkit.Rows("utf8mb4_bin utf8mb4"))
	tk.MustQuery("select * from information_schema.SESSION_VARIABLES where VARIABLE_NAME='tidb_retry_limit';").Check(testkit.Rows("tidb_retry_limit 10"))
	tk.MustQuery("select * from information_schema.ENGINES;").Check(testkit.Rows("InnoDB DEFAULT Supports transactions, row-level locking, and foreign keys YES YES YES"))
	tk.MustQuery("select * from information_schema.TABLE_CONSTRAINTS where TABLE_NAME='gc_delete_range';").Check(testkit.Rows("def mysql delete_range_index mysql gc_delete_range UNIQUE"))
	tk.MustQuery("select * from information_schema.KEY_COLUMN_USAGE where TABLE_NAME='stats_meta' and COLUMN_NAME='table_id';").Check(
		testkit.Rows("def mysql tbl def mysql stats_meta table_id 1 <nil> <nil> <nil> <nil>"))
	tk.MustQuery("select * from information_schema.STATISTICS where TABLE_NAME='columns_priv' and COLUMN_NAME='Host';").Check(
		testkit.Rows("def mysql columns_priv 0 mysql PRIMARY 1 Host A <nil> <nil> <nil>  BTREE  "))

	//test the privilege of new user for information_schema
	tk.MustExec("create user tester1")
	tk1 := testkit.NewTestKit(c, s.store)
	tk1.MustExec("use information_schema")
	c.Assert(tk1.Se.Auth(&auth.UserIdentity{
		Username: "tester1",
		Hostname: "127.0.0.1",
	}, nil, nil), IsTrue)
	tk1.MustQuery("select * from information_schema.STATISTICS;").Check([][]interface{}{})

	//test the privilege of user with some privilege for information_schema
	tk.MustExec("create user tester2")
	tk.MustExec("CREATE ROLE r_columns_priv;")
	tk.MustExec("GRANT ALL PRIVILEGES ON mysql.columns_priv TO r_columns_priv;")
	tk.MustExec("GRANT r_columns_priv TO tester2;")
	tk2 := testkit.NewTestKit(c, s.store)
	tk2.MustExec("use information_schema")
	c.Assert(tk2.Se.Auth(&auth.UserIdentity{
		Username: "tester2",
		Hostname: "127.0.0.1",
	}, nil, nil), IsTrue)
	tk2.MustExec("set role r_columns_priv")
	tk2.MustQuery("select * from information_schema.STATISTICS where TABLE_NAME='columns_priv' and COLUMN_NAME='Host';").Check(
		testkit.Rows("def mysql columns_priv 0 mysql PRIMARY 1 Host A <nil> <nil> <nil>  BTREE  "))
	tk2.MustQuery("select * from information_schema.STATISTICS where TABLE_NAME='tables_priv' and COLUMN_NAME='Host';").Check(
		[][]interface{}{})

	//test the privilege of user with all privilege for information_schema
	tk.MustExec("create user tester3")
	tk.MustExec("CREATE ROLE r_all_priv;")
	tk.MustExec("GRANT ALL PRIVILEGES ON mysql.* TO r_all_priv;")
	tk.MustExec("GRANT r_all_priv TO tester3;")
	tk3 := testkit.NewTestKit(c, s.store)
	tk3.MustExec("use information_schema")
	c.Assert(tk3.Se.Auth(&auth.UserIdentity{
		Username: "tester3",
		Hostname: "127.0.0.1",
	}, nil, nil), IsTrue)
	tk3.MustExec("set role r_all_priv")
	tk3.MustQuery("select * from information_schema.STATISTICS where TABLE_NAME='columns_priv' and COLUMN_NAME='Host';").Check(
		testkit.Rows("def mysql columns_priv 0 mysql PRIMARY 1 Host A <nil> <nil> <nil>  BTREE  "))
	tk3.MustQuery("select * from information_schema.STATISTICS where TABLE_NAME='tables_priv' and COLUMN_NAME='Host';").Check(
		testkit.Rows("def mysql tables_priv 0 mysql PRIMARY 1 Host A <nil> <nil> <nil>  BTREE  "))

	sm := &mockSessionManager{make(map[uint64]*util.ProcessInfo, 2)}
	sm.processInfoMap[1] = &util.ProcessInfo{
		ID:      1,
		User:    "user-1",
		Host:    "localhost",
		DB:      "information_schema",
		Command: byte(1),
		State:   1,
		Info:    "do something",
		StmtCtx: tk.Se.GetSessionVars().StmtCtx,
	}
	sm.processInfoMap[2] = &util.ProcessInfo{
		ID:      2,
		User:    "user-2",
		Host:    "localhost",
		DB:      "test",
		Command: byte(2),
		State:   2,
		Info:    strings.Repeat("x", 101),
		StmtCtx: tk.Se.GetSessionVars().StmtCtx,
	}
	tk.Se.SetSessionManager(sm)
	tk.MustQuery("select * from information_schema.PROCESSLIST order by ID;").Sort().Check(
		testkit.Rows(
			fmt.Sprintf("1 user-1 localhost information_schema Quit 9223372036 1 %s 0 ", "do something"),
			fmt.Sprintf("2 user-2 localhost test Init DB 9223372036 2 %s 0 ", strings.Repeat("x", 101)),
		))
	tk.MustQuery("SHOW PROCESSLIST;").Sort().Check(
		testkit.Rows(
			fmt.Sprintf("1 user-1 localhost information_schema Quit 9223372036 1 %s", "do something"),
			fmt.Sprintf("2 user-2 localhost test Init DB 9223372036 2 %s", strings.Repeat("x", 100)),
		))
	tk.MustQuery("SHOW FULL PROCESSLIST;").Sort().Check(
		testkit.Rows(
			fmt.Sprintf("1 user-1 localhost information_schema Quit 9223372036 1 %s", "do something"),
			fmt.Sprintf("2 user-2 localhost test Init DB 9223372036 2 %s", strings.Repeat("x", 101)),
		))

	sm = &mockSessionManager{make(map[uint64]*util.ProcessInfo, 2)}
	sm.processInfoMap[1] = &util.ProcessInfo{
		ID:      1,
		User:    "user-1",
		Host:    "localhost",
		DB:      "information_schema",
		Command: byte(1),
		State:   1,
		StmtCtx: tk.Se.GetSessionVars().StmtCtx,
	}
	sm.processInfoMap[2] = &util.ProcessInfo{
		ID:            2,
		User:          "user-2",
		Host:          "localhost",
		Command:       byte(2),
		State:         2,
		Info:          strings.Repeat("x", 101),
		StmtCtx:       tk.Se.GetSessionVars().StmtCtx,
		CurTxnStartTS: 410090409861578752,
	}
	tk.Se.SetSessionManager(sm)
	tk.Se.GetSessionVars().TimeZone = time.UTC
	tk.MustQuery("select * from information_schema.PROCESSLIST order by ID;").Check(
		testkit.Rows(
			fmt.Sprintf("1 user-1 localhost information_schema Quit 9223372036 1 %s 0 ", "<nil>"),
			fmt.Sprintf("2 user-2 localhost <nil> Init DB 9223372036 2 %s 0 07-29 03:26:05.158(410090409861578752)", strings.Repeat("x", 101)),
		))
	tk.MustQuery("SHOW PROCESSLIST;").Sort().Check(
		testkit.Rows(
			fmt.Sprintf("1 user-1 localhost information_schema Quit 9223372036 1 %s", "<nil>"),
			fmt.Sprintf("2 user-2 localhost <nil> Init DB 9223372036 2 %s", strings.Repeat("x", 100)),
		))
	tk.MustQuery("SHOW FULL PROCESSLIST;").Sort().Check(
		testkit.Rows(
			fmt.Sprintf("1 user-1 localhost information_schema Quit 9223372036 1 %s", "<nil>"),
			fmt.Sprintf("2 user-2 localhost <nil> Init DB 9223372036 2 %s", strings.Repeat("x", 101)),
		))
	tk.MustQuery("select * from information_schema.PROCESSLIST where db is null;").Check(
		testkit.Rows(
			fmt.Sprintf("2 user-2 localhost <nil> Init DB 9223372036 2 %s 0 07-29 03:26:05.158(410090409861578752)", strings.Repeat("x", 101)),
		))
	tk.MustQuery("select * from information_schema.PROCESSLIST where Info is null;").Check(
		testkit.Rows(
			fmt.Sprintf("1 user-1 localhost information_schema Quit 9223372036 1 %s 0 ", "<nil>"),
		))
}

func (s *testTableSuite) TestSchemataCharacterSet(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec("CREATE DATABASE `foo` DEFAULT CHARACTER SET = 'utf8mb4'")
	tk.MustQuery("select default_character_set_name, default_collation_name FROM information_schema.SCHEMATA  WHERE schema_name = 'foo'").Check(
		testkit.Rows("utf8mb4 utf8mb4_bin"))
	tk.MustExec("drop database `foo`")
}

func (s *testTableSuite) TestProfiling(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	tk.MustQuery("select * from information_schema.profiling").Check(testkit.Rows())
	tk.MustExec("set @@profiling=1")
	tk.MustQuery("select * from information_schema.profiling").Check(testkit.Rows("0 0  0 0 0 0 0 0 0 0 0 0 0 0   0"))
}

func (s *testTableSuite) TestViews(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec("CREATE DEFINER='root'@'localhost' VIEW test.v1 AS SELECT 1")
	tk.MustQuery("SELECT * FROM information_schema.views WHERE table_schema='test' AND table_name='v1'").Check(testkit.Rows("def test v1 SELECT 1 CASCADED NO root@localhost DEFINER utf8mb4 utf8mb4_bin"))
	tk.MustQuery("SELECT table_catalog, table_schema, table_name, table_type, engine, version, row_format, table_rows, avg_row_length, data_length, max_data_length, index_length, data_free, auto_increment, update_time, check_time, table_collation, checksum, create_options, table_comment FROM information_schema.tables WHERE table_schema='test' AND table_name='v1'").Check(testkit.Rows("def test v1 VIEW <nil> <nil> <nil> <nil> <nil> <nil> <nil> <nil> <nil> <nil> <nil> <nil> <nil> <nil> <nil> VIEW"))
}

func (s *testTableSuite) TestTableIDAndIndexID(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec("drop table if exists test.t")
	tk.MustExec("create table test.t (a int, b int, primary key(a), key k1(b))")
	tblID, err := strconv.Atoi(tk.MustQuery("select tidb_table_id from information_schema.tables where table_schema = 'test' and table_name = 't'").Rows()[0][0].(string))
	c.Assert(err, IsNil)
	c.Assert(tblID, Greater, 0)
	tk.MustQuery("select * from information_schema.tidb_indexes where table_schema = 'test' and table_name = 't'").Check(testkit.Rows("test t 0 PRIMARY 1 a <nil>  0", "test t 1 k1 1 b <nil>  1"))
}

func prepareSlowLogfile(c *C, slowLogFileName string) {
	f, err := os.OpenFile(slowLogFileName, os.O_CREATE|os.O_WRONLY, 0644)
	c.Assert(err, IsNil)
	_, err = f.Write([]byte(`# Time: 2019-02-12T19:33:56.571953+08:00
# Txn_start_ts: 406315658548871171
# User: root@127.0.0.1
# Conn_ID: 6
# Query_time: 4.895492
# Parse_time: 0.4
# Compile_time: 0.2
# Request_count: 1 Prewrite_time: 0.19 Commit_time: 0.01 Commit_backoff_time: 0.18 Backoff_types: [txnLock] Resolve_lock_time: 0.03 Write_keys: 15 Write_size: 480 Prewrite_region: 1 Txn_retry: 8
# Process_time: 0.161 Request_count: 1 Total_keys: 100001 Process_keys: 100000
# Wait_time: 0.101
# Backoff_time: 0.092
# DB: test
# Is_internal: false
# Digest: 42a1c8aae6f133e934d4bf0147491709a8812ea05ff8819ec522780fe657b772
# Stats: t1:1,t2:2
# Cop_proc_avg: 0.1 Cop_proc_p90: 0.2 Cop_proc_max: 0.03 Cop_proc_addr: 127.0.0.1:20160
# Cop_wait_avg: 0.05 Cop_wait_p90: 0.6 Cop_wait_max: 0.8 Cop_wait_addr: 0.0.0.0:20160
# Mem_max: 70724
# Succ: true
# Plan: abcd
# Prev_stmt: update t set i = 2;
select * from t_slim;`))
	c.Assert(f.Sync(), IsNil)
	c.Assert(err, IsNil)
}

func (s *testTableSuite) TestTableRowIDShardingInfo(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec("DROP DATABASE IF EXISTS `sharding_info_test_db`")
	tk.MustExec("CREATE DATABASE `sharding_info_test_db`")

	assertShardingInfo := func(tableName string, expectInfo interface{}) {
		querySQL := fmt.Sprintf("select tidb_row_id_sharding_info from information_schema.tables where table_schema = 'sharding_info_test_db' and table_name = '%s'", tableName)
		info := tk.MustQuery(querySQL).Rows()[0][0]
		if expectInfo == nil {
			c.Assert(info, Equals, "<nil>")
		} else {
			c.Assert(info, Equals, expectInfo)
		}
	}
	tk.MustExec("CREATE TABLE `sharding_info_test_db`.`t1` (a int)")
	assertShardingInfo("t1", "NOT_SHARDED")

	tk.MustExec("CREATE TABLE `sharding_info_test_db`.`t2` (a int key)")
	assertShardingInfo("t2", "NOT_SHARDED(PK_IS_HANDLE)")

	tk.MustExec("CREATE TABLE `sharding_info_test_db`.`t3` (a int) SHARD_ROW_ID_BITS=4")
	assertShardingInfo("t3", "SHARD_BITS=4")

	tk.MustExec("CREATE VIEW `sharding_info_test_db`.`tv` AS select 1")
	assertShardingInfo("tv", nil)

	testFunc := func(dbName string, expectInfo interface{}) {
		dbInfo := model.DBInfo{Name: model.NewCIStr(dbName)}
		tableInfo := model.TableInfo{}

		info := infoschema.GetShardingInfo(&dbInfo, &tableInfo)
		c.Assert(info, Equals, expectInfo)
	}

	testFunc("information_schema", nil)
	testFunc("mysql", nil)
	testFunc("performance_schema", nil)
	testFunc("uucc", "NOT_SHARDED")

	tk.MustExec("DROP DATABASE `sharding_info_test_db`")
}

func (s *testTableSuite) TestSlowQuery(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	// Prepare slow log file.
	slowLogFileName := "tidb_slow.log"
	prepareSlowLogfile(c, slowLogFileName)
	defer os.Remove(slowLogFileName)

	tk.MustExec(fmt.Sprintf("set @@tidb_slow_query_file='%v'", slowLogFileName))
	tk.MustExec("set time_zone = '+08:00';")
	re := tk.MustQuery("select * from information_schema.slow_query")
	re.Check(testutil.RowsWithSep("|",
		"2019-02-12 19:33:56.571953|406315658548871171|root|127.0.0.1|6|4.895492|0.4|0.2|0.19|0.01|0|0.18|[txnLock]|0.03|0|15|480|1|8|0.161|0.101|0.092|1|100001|100000|test||0|42a1c8aae6f133e934d4bf0147491709a8812ea05ff8819ec522780fe657b772|t1:1,t2:2|0.1|0.2|0.03|127.0.0.1:20160|0.05|0.6|0.8|0.0.0.0:20160|70724|1|abcd|update t set i = 2;|select * from t_slim;"))
	tk.MustExec("set time_zone = '+00:00';")
	re = tk.MustQuery("select * from information_schema.slow_query")
	re.Check(testutil.RowsWithSep("|", "2019-02-12 11:33:56.571953|406315658548871171|root|127.0.0.1|6|4.895492|0.4|0.2|0.19|0.01|0|0.18|[txnLock]|0.03|0|15|480|1|8|0.161|0.101|0.092|1|100001|100000|test||0|42a1c8aae6f133e934d4bf0147491709a8812ea05ff8819ec522780fe657b772|t1:1,t2:2|0.1|0.2|0.03|127.0.0.1:20160|0.05|0.6|0.8|0.0.0.0:20160|70724|1|abcd|update t set i = 2;|select * from t_slim;"))

	// Test for long query.
	f, err := os.OpenFile(slowLogFileName, os.O_CREATE|os.O_WRONLY, 0644)
	c.Assert(err, IsNil)
	defer f.Close()
	_, err = f.Write([]byte(`
# Time: 2019-02-13T19:33:56.571953+08:00
`))
	c.Assert(err, IsNil)
	sql := "select * from "
	for len(sql) < 5000 {
		sql += "abcdefghijklmnopqrstuvwxyz_1234567890_qwertyuiopasdfghjklzxcvbnm"
	}
	sql += ";"
	_, err = f.Write([]byte(sql))
	c.Assert(err, IsNil)
	c.Assert(f.Close(), IsNil)
	re = tk.MustQuery("select query from information_schema.slow_query order by time desc limit 1")
	rows := re.Rows()
	c.Assert(rows[0][0], Equals, sql)
}

func (s *testTableSuite) TestForAnalyzeStatus(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	statistics.ClearHistoryJobs()
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t (a int, b int, index idx(a))")
	tk.MustExec("insert into t values (1,2),(3,4)")
	tk.MustExec("analyze table t")

	result := tk.MustQuery("select * from information_schema.analyze_status").Sort()

	c.Assert(len(result.Rows()), Equals, 2)
	c.Assert(result.Rows()[0][0], Equals, "test")
	c.Assert(result.Rows()[0][1], Equals, "t")
	c.Assert(result.Rows()[0][2], Equals, "")
	c.Assert(result.Rows()[0][3], Equals, "analyze columns")
	c.Assert(result.Rows()[0][4], Equals, "2")
	c.Assert(result.Rows()[0][5], NotNil)
	c.Assert(result.Rows()[0][6], Equals, "finished")

	c.Assert(len(result.Rows()), Equals, 2)
	c.Assert(result.Rows()[1][0], Equals, "test")
	c.Assert(result.Rows()[1][1], Equals, "t")
	c.Assert(result.Rows()[1][2], Equals, "")
	c.Assert(result.Rows()[1][3], Equals, "analyze index idx")
	c.Assert(result.Rows()[1][4], Equals, "2")
	c.Assert(result.Rows()[1][5], NotNil)
	c.Assert(result.Rows()[1][6], Equals, "finished")
}

func (s *testTableSuite) TestForServersInfo(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	result := tk.MustQuery("select * from information_schema.TIDB_SERVERS_INFO")
	c.Assert(len(result.Rows()), Equals, 1)

	serversInfo, err := infosync.GetAllServerInfo(context.Background())
	c.Assert(err, IsNil)
	c.Assert(len(serversInfo), Equals, 1)

	for _, info := range serversInfo {
		c.Assert(result.Rows()[0][0], Equals, info.ID)
		c.Assert(result.Rows()[0][1], Equals, info.IP)
		c.Assert(result.Rows()[0][2], Equals, strconv.FormatInt(int64(info.Port), 10))
		c.Assert(result.Rows()[0][3], Equals, strconv.FormatInt(int64(info.StatusPort), 10))
		c.Assert(result.Rows()[0][4], Equals, info.Lease)
		c.Assert(result.Rows()[0][5], Equals, info.Version)
		c.Assert(result.Rows()[0][6], Equals, info.GitHash)
	}
}

func (s *testTableSuite) TestColumnStatistics(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	tk.MustQuery("select * from information_schema.column_statistics").Check(testkit.Rows())
}

type mockStore struct {
	tikv.Storage
	host string
}

func (s *mockStore) EtcdAddrs() []string    { return []string{s.host} }
func (s *mockStore) TLSConfig() *tls.Config { panic("not implemented") }
func (s *mockStore) StartGCWorker() error   { panic("not implemented") }

func (s *testClusterTableSuite) TestTiDBClusterInfo(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	err := tk.QueryToErr("select * from information_schema.cluster_info")
	c.Assert(err, NotNil)
	mockAddr := s.mockAddr
	store := &mockStore{
		s.store.(tikv.Storage),
		mockAddr,
	}
	tk = testkit.NewTestKit(c, store)
	tk.MustQuery("select * from information_schema.cluster_info").Check(testkit.Rows(
		"tidb :4000 :10080 5.7.25-TiDB-None None",
		"pd "+mockAddr+" "+mockAddr+" 4.0.0-alpha mock-pd-githash",
		"tikv 127.0.0.1:20160 "+mockAddr+" 4.0.0-alpha mock-tikv-githash",
	))

	instances := []string{
		"pd,127.0.0.1:11080," + mockAddr + ",mock-version,mock-githash",
		"tidb,127.0.0.1:11080," + mockAddr + ",mock-version,mock-githash",
		"tikv,127.0.0.1:11080," + mockAddr + ",mock-version,mock-githash",
	}
	fpExpr := `return("` + strings.Join(instances, ";") + `")`
	c.Assert(failpoint.Enable("github.com/pingcap/tidb/infoschema/mockClusterInfo", fpExpr), IsNil)
	defer func() { c.Assert(failpoint.Disable("github.com/pingcap/tidb/infoschema/mockClusterInfo"), IsNil) }()
	tk.MustQuery("select * from information_schema.cluster_config").Check(testkit.Rows(
		"pd 127.0.0.1:11080 key1 value1",
		"pd 127.0.0.1:11080 key2.nest1 n-value1",
		"pd 127.0.0.1:11080 key2.nest2 n-value2",
		"pd 127.0.0.1:11080 key3.key4.nest3 n-value4",
		"pd 127.0.0.1:11080 key3.key4.nest4 n-value5",
		"pd 127.0.0.1:11080 key3.nest1 n-value1",
		"pd 127.0.0.1:11080 key3.nest2 n-value2",
		"tidb 127.0.0.1:11080 key1 value1",
		"tidb 127.0.0.1:11080 key2.nest1 n-value1",
		"tidb 127.0.0.1:11080 key2.nest2 n-value2",
		"tidb 127.0.0.1:11080 key3.key4.nest3 n-value4",
		"tidb 127.0.0.1:11080 key3.key4.nest4 n-value5",
		"tidb 127.0.0.1:11080 key3.nest1 n-value1",
		"tidb 127.0.0.1:11080 key3.nest2 n-value2",
		"tikv 127.0.0.1:11080 key1 value1",
		"tikv 127.0.0.1:11080 key2.nest1 n-value1",
		"tikv 127.0.0.1:11080 key2.nest2 n-value2",
		"tikv 127.0.0.1:11080 key3.key4.nest3 n-value4",
		"tikv 127.0.0.1:11080 key3.key4.nest4 n-value5",
		"tikv 127.0.0.1:11080 key3.nest1 n-value1",
		"tikv 127.0.0.1:11080 key3.nest2 n-value2",
	))
	tk.MustQuery("select TYPE, `KEY`, VALUE from information_schema.cluster_config where `key`='key3.key4.nest4' order by type").Check(testkit.Rows(
		"pd key3.key4.nest4 n-value5",
		"tidb key3.key4.nest4 n-value5",
		"tikv key3.key4.nest4 n-value5",
	))
}

func (s *testTableSuite) TestReloadDropDatabase(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec("create database test_dbs")
	tk.MustExec("use test_dbs")
	tk.MustExec("create table t1 (a int)")
	tk.MustExec("create table t2 (a int)")
	tk.MustExec("create table t3 (a int)")
	is := domain.GetDomain(tk.Se).InfoSchema()
	t2, err := is.TableByName(model.NewCIStr("test_dbs"), model.NewCIStr("t2"))
	c.Assert(err, IsNil)
	tk.MustExec("drop database test_dbs")
	is = domain.GetDomain(tk.Se).InfoSchema()
	_, err = is.TableByName(model.NewCIStr("test_dbs"), model.NewCIStr("t2"))
	c.Assert(terror.ErrorEqual(infoschema.ErrTableNotExists, err), IsTrue)
	_, ok := is.TableByID(t2.Meta().ID)
	c.Assert(ok, IsFalse)
}

func (s *testTableSuite) TestForTableTiFlashReplica(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	statistics.ClearHistoryJobs()
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t")
	tk.MustExec("create table t (a int, b int, index idx(a))")
	tk.MustExec("alter table t set tiflash replica 2 location labels 'a','b';")
	tk.MustQuery("select TABLE_SCHEMA,TABLE_NAME,REPLICA_COUNT,LOCATION_LABELS,AVAILABLE from information_schema.tiflash_replica").Check(testkit.Rows("test t 2 a,b 0"))
	tbl, err := domain.GetDomain(tk.Se).InfoSchema().TableByName(model.NewCIStr("test"), model.NewCIStr("t"))
	c.Assert(err, IsNil)
	tbl.Meta().TiFlashReplica.Available = true
	tk.MustQuery("select TABLE_SCHEMA,TABLE_NAME,REPLICA_COUNT,LOCATION_LABELS,AVAILABLE from information_schema.tiflash_replica").Check(testkit.Rows("test t 2 a,b 1"))
}

func (s *testClusterTableSuite) TestForClusterServerInfo(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	mockAddr := "127.0.0.1:10080"
	instances := []string{
		"pd,127.0.0.1:11080," + mockAddr + ",mock-version,mock-githash",
		"tidb,127.0.0.1:11080," + mockAddr + ",mock-version,mock-githash",
		"tikv,127.0.0.1:11080," + mockAddr + ",mock-version,mock-githash",
	}
	fpExpr := `return("` + strings.Join(instances, ";") + `")`
	c.Assert(failpoint.Enable("github.com/pingcap/tidb/infoschema/mockClusterInfo", fpExpr), IsNil)
	defer func() { c.Assert(failpoint.Disable("github.com/pingcap/tidb/infoschema/mockClusterInfo"), IsNil) }()

	re := tk.MustQuery("select * from information_schema.CLUSTER_LOAD;")
	rows := re.Rows()
	c.Assert(len(rows), Greater, 0)

	// Currently only TiDB implement this.
	// TODO: fix me after tikv/pd server support this.
	typeMap := map[string]struct{}{
		"tidb": {},
	}
	addrMap := map[string]struct{}{
		"127.0.0.1:10080": {},
	}
	nameMap := map[string]struct{}{
		"cpu":  {},
		"mem":  {},
		"net":  {},
		"disk": {},
	}
	for _, row := range rows {
		tp := row[0].(string)
		addr := row[1].(string)
		name := row[2].(string)
		delete(typeMap, tp)
		delete(addrMap, addr)
		delete(nameMap, name)
	}
	c.Assert(len(typeMap), Equals, 0)
	c.Assert(len(addrMap), Equals, 0)
	c.Assert(len(nameMap), Equals, 0)
}

func (s *testTableSuite) TestSystemSchemaID(c *C) {
	uniqueIDMap := make(map[int64]string)
	s.checkSystemSchemaTableID(c, "information_schema", autoid.SystemSchemaIDFlag|1, 1, 10000, uniqueIDMap)
	s.checkSystemSchemaTableID(c, "performance_schema", autoid.SystemSchemaIDFlag|10000, 10000, 20000, uniqueIDMap)
}

func (s *testTableSuite) checkSystemSchemaTableID(c *C, dbName string, dbID, start, end int64, uniqueIDMap map[int64]string) {
	is := s.dom.InfoSchema()
	c.Assert(is, NotNil)
	db, ok := is.SchemaByName(model.NewCIStr(dbName))
	c.Assert(ok, IsTrue)
	c.Assert(db.ID, Equals, dbID)
	// Test for information_schema table id.
	tables := is.SchemaTables(model.NewCIStr(dbName))
	c.Assert(len(tables), Greater, 0)
	for _, tbl := range tables {
		tid := tbl.Meta().ID
		comment := Commentf("table name is %v", tbl.Meta().Name)
		c.Assert(tid&autoid.SystemSchemaIDFlag, Greater, int64(0), comment)
		c.Assert(tid&^autoid.SystemSchemaIDFlag, Greater, start, comment)
		c.Assert(tid&^autoid.SystemSchemaIDFlag, Less, end, comment)
		name, ok := uniqueIDMap[tid]
		c.Assert(ok, IsFalse, Commentf("schema id of %v is duplicate with %v, both is %v", name, tbl.Meta().Name, tid))
		uniqueIDMap[tid] = tbl.Meta().Name.O
	}
}

func (s *testClusterTableSuite) TestSelectClusterTable(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	slowLogFileName := "tidb-slow.log"
	prepareSlowLogfile(c, slowLogFileName)
	defer os.Remove(slowLogFileName)
	tk.MustExec("use information_schema")
	tk.MustQuery("select count(*) from `CLUSTER_SLOW_QUERY`").Check(testkit.Rows("1"))
	tk.MustQuery("select count(*) from `CLUSTER_PROCESSLIST`").Check(testkit.Rows("1"))
	tk.MustQuery("select * from `CLUSTER_PROCESSLIST`").Check(testkit.Rows("1 root 127.0.0.1 <nil> Query 9223372036 0 <nil> 0  :10080"))
	tk.MustQuery("select query_time, conn_id from `CLUSTER_SLOW_QUERY` order by time limit 1").Check(testkit.Rows("4.895492 6"))
	tk.MustQuery("select count(*) from `CLUSTER_SLOW_QUERY` group by digest").Check(testkit.Rows("1"))
	tk.MustQuery("select digest, count(*) from `CLUSTER_SLOW_QUERY` group by digest").Check(testkit.Rows("42a1c8aae6f133e934d4bf0147491709a8812ea05ff8819ec522780fe657b772 1"))
	tk.MustQuery("select count(*) from `CLUSTER_SLOW_QUERY` where time > now() group by digest").Check(testkit.Rows())
}
