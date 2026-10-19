package sqlfilter

import (
	"errors"
	"strings"
	"testing"
)

func run(t *testing.T, f *Filter, in string) string {
	t.Helper()
	var out strings.Builder
	if err := f.Copy(&out, strings.NewReader(in)); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// lines as written by mariadb-dump 11.8, mysqldump 8.4 and phpMyAdmin
const dump = "-- MariaDB dump 10.19  Distrib 11.8.6-MariaDB\n" +
	"-- Host: localhost    Database: shop\n" +
	"/*!50003 CREATE*/ /*!50017 DEFINER=`root`@`localhost`*/ /*!50003 trigger tr before insert on t for each row set new.a = 1 */;;\n" +
	"CREATE DEFINER=`app`@`%` PROCEDURE `p`()\n" +
	"select 'DEFINER=`x`@`y`'\n" +
	"/*!50106 CREATE*/ /*!50117 DEFINER='old'@'10.0.0.%'*/ /*!50106 EVENT `e` ON SCHEDULE EVERY 1 DAY DO select 1 */ ;;\n" +
	"/*!50013 DEFINER=`root`@`localhost` SQL SECURITY DEFINER */\n" +
	"/*!50001 VIEW `v` AS select `shop`.`t`.`a` AS `a` from `t` */;\n" +
	"CREATE ALGORITHM=UNDEFINED DEFINER=root@localhost SQL SECURITY DEFINER VIEW `w` AS SELECT `shop`.`t`.`a` AS `a` FROM `shop`.`t` ;\n" +
	"INSERT INTO `t` VALUES ('DEFINER=`root`@`localhost`','`shop`.x VIEW ');\n" +
	"CREATE DEFINER=CURRENT_USER FUNCTION f() RETURNS int RETURN 1;\n"

func TestRewrites(t *testing.T) {
	f := &Filter{Staging: "_mdb_staging_7"}
	got := run(t, f, dump)
	if f.Source != "shop" {
		t.Fatalf("source %q", f.Source)
	}
	for _, want := range []string{
		"/*!50003 CREATE*/ /*!50017 DEFINER=CURRENT_ROLE*/ /*!50003 trigger",
		"CREATE DEFINER=CURRENT_ROLE PROCEDURE `p`()\n",
		"select 'DEFINER=`x`@`y`'\n",
		"/*!50117 DEFINER=CURRENT_ROLE*/ /*!50106 EVENT",
		"/*!50013 DEFINER=CURRENT_ROLE SQL SECURITY DEFINER */\n",
		"/*!50001 VIEW `v` AS select `_mdb_staging_7`.`t`.`a` AS `a` from `t` */;\n",
		"DEFINER=CURRENT_ROLE SQL SECURITY DEFINER VIEW `w` AS SELECT `_mdb_staging_7`.`t`.`a` AS `a` FROM `_mdb_staging_7`.`t` ;\n",
		"INSERT INTO `t` VALUES ('DEFINER=`root`@`localhost`','`shop`.x VIEW ');\n",
		"CREATE DEFINER=CURRENT_ROLE FUNCTION f()",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if strings.Contains(got, "root`@") && strings.Count(got, "root`@") != 1 {
		t.Errorf("definer left:\n%s", got)
	}
}

func TestDatabaseStatements(t *testing.T) {
	in := "CREATE DATABASE /*!32312 IF NOT EXISTS*/ `shop` /*!40100 DEFAULT CHARACTER SET utf8mb4 */;\nUSE `shop`;\nCREATE TABLE `t` (a int);\n"
	f := &Filter{Staging: "s"}
	got := run(t, f, in)
	if strings.Contains(got, "USE `shop`") || strings.Contains(got, "CREATE DATABASE /*") || !strings.Contains(got, "CREATE TABLE `t`") || f.Source != "shop" {
		t.Fatalf("got %q source %q", got, f.Source)
	}
	var out strings.Builder
	err := (&Filter{}).Copy(&out, strings.NewReader(in+"USE `other`;\n"))
	if !errors.Is(err, ErrManyDatabases) {
		t.Fatalf("err = %v", err)
	}
	// a snapshot names the database it was taken from, the dump has no USE
	f = &Filter{Source: "shop", Staging: "s"}
	if got := run(t, f, "USE `shop`;\n"); got != "-- dropped by the panel: USE\n" {
		t.Fatalf("got %q", got)
	}
}

func TestGTIDDropped(t *testing.T) {
	in := "SET @@SESSION.SQL_LOG_BIN= 0;\nSET @@GLOBAL.GTID_PURGED=/*!80000 '+'*/ 'a:1-5,\nb:1-3';\nCREATE TABLE t (a int);\n"
	got := run(t, &Filter{}, in)
	if strings.Contains(got, "GTID") || strings.Contains(got, "b:1-3") || strings.Contains(got, "SQL_LOG_BIN") || !strings.HasSuffix(got, "CREATE TABLE t (a int);\n") {
		t.Fatalf("got %q", got)
	}
}

func TestLongLinesAndTables(t *testing.T) {
	long := "INSERT INTO `big` VALUES ('" + strings.Repeat("DEFINER=`root`@`x` ", 200000) + "');\n"
	var tables []string
	f := &Filter{OnTable: func(n string) { tables = append(tables, n) }}
	got := run(t, f, "-- Dumping data for table `a``b`\n"+long+"CREATE DEFINER=`r`@`h` VIEW v AS SELECT 1;")
	if !strings.Contains(got, long) || !strings.HasSuffix(got, "CREATE DEFINER=CURRENT_ROLE VIEW v AS SELECT 1;") {
		t.Fatal("long line changed or last line without newline not filtered")
	}
	if len(tables) != 1 || tables[0] != "a`b" {
		t.Fatalf("tables %q", tables)
	}
}
