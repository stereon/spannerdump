package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"cloud.google.com/go/spanner"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"

	adminapi "cloud.google.com/go/spanner/admin/database/apiv1"
	adminpb "google.golang.org/genproto/googleapis/spanner/admin/database/v1"
	sppb "google.golang.org/genproto/googleapis/spanner/v1"
)

// This is an ad hoc value, but considering mutations limit (20,000),
// 100 rows/statement would be safe in most cases.
// https://cloud.google.com/spanner/quotas#limits_for_creating_reading_updating_and_deleting_data
const defaultBulkSize = 100

// Dumper is a dumper to export a database.
type Dumper struct {
	project   string
	instance  string
	database  string
	tables    map[string]bool
	out       io.Writer
	timestamp *time.Time
	bulkSize  uint
	querySql  string
	format    string

	client      *spanner.Client
	adminClient *adminapi.DatabaseAdminClient
}

// NewDumper creates Dumper with specified configurations.
func NewDumper(ctx context.Context, project, instance, database string, out io.Writer, timestamp *time.Time, bulkSize uint, tables []string, querySql string, format string) (*Dumper, error) {
	dbPath := fmt.Sprintf("projects/%s/instances/%s/databases/%s", project, instance, database)
	client, err := spanner.NewClientWithConfig(ctx, dbPath, spanner.ClientConfig{
		SessionPoolConfig: spanner.SessionPoolConfig{
			MinOpened: 1,
			MaxOpened: 1,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create spanner client: %v", err)
	}

	var opts []option.ClientOption
	if emulatorAddr := os.Getenv("SPANNER_EMULATOR_HOST"); emulatorAddr != "" {
		emulatorOpts := []option.ClientOption{
			option.WithEndpoint(emulatorAddr),
			option.WithGRPCDialOption(grpc.WithInsecure()),
			option.WithoutAuthentication(),
		}
		opts = append(opts, emulatorOpts...)
	}
	adminClient, err := adminapi.NewDatabaseAdminClient(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create spanner admin client: %v", err)
	}

	if bulkSize == 0 {
		bulkSize = defaultBulkSize
	}

	d := &Dumper{
		project:     project,
		instance:    instance,
		database:    database,
		tables:      map[string]bool{},
		out:         out,
		timestamp:   timestamp,
		bulkSize:    bulkSize,
		client:      client,
		querySql:    querySql,
		format:      format,
		adminClient: adminClient,
	}

	for _, table := range tables {
		d.tables[strings.Trim(table, "`")] = true
	}
	return d, nil
}

// Cleanup cleans up hold resources.
func (d *Dumper) Cleanup() {
	d.client.Close()
	d.adminClient.Close()
}

// DumpDDLs dumps all DDLs in the database.
func (d *Dumper) DumpDDLs(ctx context.Context) error {
	dbPath := fmt.Sprintf("projects/%s/instances/%s/databases/%s", d.project, d.instance, d.database)
	resp, err := d.adminClient.GetDatabaseDdl(ctx, &adminpb.GetDatabaseDdlRequest{
		Database: dbPath,
	})
	if err != nil {
		return err
	}

	for _, ddl := range resp.Statements {
		if len(d.tables) > 0 && !d.tables[parseTableNameFromDDL(ddl)] {
			continue
		}
		fmt.Fprintf(d.out, "%s;\n", ddl)
	}

	return nil
}

func parseTableNameFromDDL(ddl string) string {
	if indexRegexp.MatchString(ddl) {
		match := indexRegexp.FindStringSubmatch(ddl)
		return match[1]
	}
	if tableRegexp.MatchString(ddl) {
		match := tableRegexp.FindStringSubmatch(ddl)
		return match[1]
	}
	if alterRegexp.MatchString(ddl) {
		match := alterRegexp.FindStringSubmatch(ddl)
		return match[1]
	}
	return ""
}

var indexRegexp = regexp.MustCompile("^\\s*CREATE\\s+(?:UNIQUE\\s+|NULL_FILTERED\\s+)?INDEX\\s+(?:[a-zA-Z0-9_`]+)\\s+ON\\s+`?([a-zA-Z0-9_]+)`?")
var tableRegexp = regexp.MustCompile("^\\s*CREATE\\s+TABLE\\s+`?([a-zA-Z0-9_]+)`?")
var alterRegexp = regexp.MustCompile("^\\s*ALTER\\s+TABLE\\s+`?([a-zA-Z0-9_]+)`?")

// DumpTables dumps all table records in the database.
func (d *Dumper) DumpTables(ctx context.Context) error {
	txn := d.client.ReadOnlyTransaction()
	if d.timestamp != nil {
		txn = txn.WithTimestampBound(spanner.ReadTimestamp(*d.timestamp))
	}
	defer txn.Close()

	iter, err := FetchTables(ctx, txn)
	if err != nil {
		return err
	}

	return iter.Do(func(t *Table) error {
		if len(d.tables) > 0 && !d.tables[t.Name] {
			return nil
		}
		return d.dumpTable(ctx, t, d.querySql, txn)
	})
}

func (d *Dumper) dumpTable(ctx context.Context, table *Table, querySql string, txn *spanner.ReadOnlyTransaction) error {
	var stmt spanner.Statement
	if querySql == "" {
		stmt = spanner.NewStatement(fmt.Sprintf("SELECT %s FROM `%s`", table.quotedColumnList(), table.Name))
	} else {
		stmt = spanner.NewStatement(fmt.Sprintf("SELECT %s FROM `%s` WHERE %s", table.quotedColumnList(), table.Name, querySql))
	}
	opts := spanner.QueryOptions{Priority: sppb.RequestOptions_PRIORITY_LOW}
	iter := txn.QueryWithOptions(ctx, stmt, opts)
	defer iter.Stop()

	writer := NewBufferedWriter(table, d.out, d.bulkSize, d.format)
	if d.format == "json" {
		defer writer.FormatJson()
	} else if d.format == "sql" {
		defer writer.FormatSql()
	} else {
		exitf("Unsupported format: %s\n", d.format)
	}
	for {
		row, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return err
		}

		values, err := DecodeRow(row)
		if err != nil {
			return err
		}
		writer.Write(values)
	}

	return nil
}
