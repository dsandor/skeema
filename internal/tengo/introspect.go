package tengo

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"

	"github.com/VividCortex/mysqlerr"
	"github.com/jmoiron/sqlx"
	"golang.org/x/sync/errgroup"
)

/*
	Important note on information_schema queries in this file: MySQL 8.0 changes
	information_schema column names to come back from queries in all caps, so we
	need to explicitly use AS clauses in order to get them back as lowercase and
	have sqlx Select() work.
*/

var reExtraOnUpdate = regexp.MustCompile(`(?i)\bon update (current_timestamp(?:\(\d*\))?)`)

func querySchemaTables(ctx context.Context, db *sqlx.DB, schema string, flavor Flavor) ([]*Table, error) {
	tables, havePartitions, err := queryTablesInSchema(ctx, db, schema, flavor)
	if err != nil {
		return nil, err
	}

	g, subCtx := errgroup.WithContext(ctx)

	for n := range tables {
		t := tables[n] // avoid issues with goroutines and loop iterator values
		g.Go(func() (err error) {
			t.CreateStatement, err = showCreateTable(subCtx, db, t.Name, schema, flavor)
			if err != nil {
				err = fmt.Errorf("Error executing SHOW CREATE TABLE for %s.%s: %s", EscapeIdentifier(schema), EscapeIdentifier(t.Name), err)
			}
			return err
		})
	}

	var columnsByTableName map[string][]*Column
	g.Go(func() (err error) {
		columnsByTableName, err = queryColumnsInSchema(subCtx, db, schema, flavor)
		return err
	})

	var primaryKeyByTableName map[string]*Index
	var secondaryIndexesByTableName map[string][]*Index
	g.Go(func() (err error) {
		primaryKeyByTableName, secondaryIndexesByTableName, err = queryIndexesInSchema(subCtx, db, schema, flavor)
		return err
	})

	var foreignKeysByTableName map[string][]*ForeignKey
	g.Go(func() (err error) {
		foreignKeysByTableName, err = queryForeignKeysInSchema(subCtx, db, schema, flavor)
		return err
	})

	var checksByTableName map[string][]*Check
	if flavor.HasCheckConstraints() {
		g.Go(func() (err error) {
			checksByTableName, err = queryChecksInSchema(subCtx, db, schema, flavor)
			return err
		})
	}

	var partitioningByTableName map[string]*TablePartitioning
	if havePartitions {
		g.Go(func() (err error) {
			partitioningByTableName, err = queryPartitionsInSchema(subCtx, db, schema, flavor)
			return err
		})
	}

	// Await all of the async queries
	if err := g.Wait(); err != nil {
		return nil, err
	}

	// Assemble all the data, fix edge cases, and determine if SHOW CREATE TABLE
	// matches expectation
	for _, t := range tables {
		t.Columns = columnsByTableName[t.Name]
		t.PrimaryKey = primaryKeyByTableName[t.Name]
		t.SecondaryIndexes = secondaryIndexesByTableName[t.Name]
		t.ForeignKeys = foreignKeysByTableName[t.Name]
		t.Checks = checksByTableName[t.Name]

		if p, ok := partitioningByTableName[t.Name]; ok {
			for _, part := range p.Partitions {
				part.Engine = t.Engine
			}
			t.Partitioning = p
			fixPartitioningEdgeCases(t, flavor)
		}

		// Obtain TABLESPACE clause from SHOW CREATE TABLE, if present
		t.Tablespace = ParseCreateTablespace(t.CreateStatement)

		// Obtain next AUTO_INCREMENT value from SHOW CREATE TABLE, which avoids
		// potential problems with information_schema discrepancies
		_, t.NextAutoIncrement = ParseCreateAutoInc(t.CreateStatement)
		if t.NextAutoIncrement == 0 && t.HasAutoIncrement() {
			t.NextAutoIncrement = 1
		}
		// Remove create options which don't affect InnoDB
		if t.Engine == "InnoDB" {
			t.CreateStatement = NormalizeCreateOptions(t.CreateStatement)
		}
		// Index order is unpredictable with new MySQL 8 data dictionary, so reorder
		// indexes based on parsing SHOW CREATE TABLE if needed
		if flavor.Min(FlavorMySQL80) && len(t.SecondaryIndexes) > 1 {
			fixIndexOrder(t)
		}
		// Foreign keys order is unpredictable in MySQL before 5.6, so reorder
		// foreign keys based on parsing SHOW CREATE TABLE if needed
		if !flavor.SortedForeignKeys() && len(t.ForeignKeys) > 1 {
			fixForeignKeyOrder(t)
		}
		// Create options order is unpredictable with the new MySQL 8 data dictionary
		// Also need to fix some charset/collation edge cases in SHOW CREATE TABLE
		// behavior in MySQL 8
		if flavor.Min(FlavorMySQL80) {
			fixCreateOptionsOrder(t, flavor)
			fixShowCharSets(t)
		}
		// MySQL 5.7+ generated column expressions must be reparased from SHOW CREATE
		// TABLE to properly obtain any 4-byte chars. Additionally in 8.0 the I_S
		// representation has incorrect escaping and potentially different charset
		// in string literal introducers.
		if flavor.Min(FlavorMySQL57) {
			fixGenerationExpr(t, flavor)
		}
		// Percona Server column compression can only be parsed from SHOW CREATE
		// TABLE. (Although it also has new I_S tables, their name differs pre-8.0
		// vs post-8.0, and cols that aren't using a COMPRESSION_DICTIONARY are not
		// even present there.)
		if flavor.Min(FlavorPercona56.Dot(33)) && strings.Contains(t.CreateStatement, "COLUMN_FORMAT COMPRESSED") {
			fixPerconaColCompression(t)
		}
		// FULLTEXT indexes may have a PARSER clause, which isn't exposed in I_S
		if strings.Contains(t.CreateStatement, "WITH PARSER") {
			fixFulltextIndexParsers(t, flavor)
		}
		// Fix problems with I_S data for default expressions as well as functional
		// indexes in MySQL 8
		if flavor.Min(FlavorMySQL80) {
			fixDefaultExpression(t, flavor)
			fixIndexExpression(t, flavor)
		}
		// Fix shortcoming in I_S data for check constraints
		if len(t.Checks) > 0 {
			fixChecks(t, flavor)
		}

		// Compare what we expect the create DDL to be, to determine if we support
		// diffing for the table. (No need to remove next AUTO_INCREMENT from this
		// comparison since the value was parsed from t.CreateStatement earlier.)
		if t.CreateStatement != t.GeneratedCreateStatement(flavor) {
			t.UnsupportedDDL = true
		}
	}
	return tables, nil
}

func queryTablesInSchema(ctx context.Context, db *sqlx.DB, schema string, flavor Flavor) ([]*Table, bool, error) {
	var rawTables []struct {
		Name               string         `db:"TABLE_NAME"`
		Type               string         `db:"TABLE_TYPE"`
		Engine             sql.NullString `db:"ENGINE"`
		TableCollation     sql.NullString `db:"TABLE_COLLATION"`
		CreateOptions      sql.NullString `db:"CREATE_OPTIONS"`
		Comment            sql.NullString `db:"TABLE_COMMENT"`
		CharSet            string         `db:"CHARACTER_SET_NAME"`
		CollationIsDefault string         `db:"IS_DEFAULT"`
	}
	var query string
	if flavor.IsSnowflake() {
		query = `SELECT 
		       t.table_name 	as TABLE_NAME, 
		       t.table_type 	as TABLE_TYPE,
		        '' 				as TABLE_COLLATION,
		       '' 				as CREATE_OPTIONS,
		       'UTF-8' 			as CHARACTER_SET_NAME, 
		       true 			as IS_DEFAULT
               , t.COMMENT 		as TABLE_COMMENT
               , 'Snowflake' 	as ENGINE
		FROM   information_schema.tables t
		WHERE  t.table_schema = ?
		AND    t.table_type = 'BASE TABLE'`
	} else {
		query = `
		SELECT SQL_BUFFER_RESULT
		       t.table_name 		AS TABLE_NAME,
		       t.table_type 		AS TABLE_TYPE,
		       t.engine 			AS ENGINE, 
		       t.table_collation 	AS TABLE_COLLATION,
		       t.create_options 	AS CREATE_OPTIONS, 
		       t.table_comment 		AS TABLE_COMMENT,
		       c.character_set_name AS CHARACTER_SET_NAME, 
		       c.is_default 		AS IS_DEFAULT
		FROM   information_schema.tables t
		JOIN   information_schema.collations c ON t.table_collation = c.collation_name
		WHERE  t.table_schema = ?
		AND    t.table_type = 'BASE TABLE'`

	}
	if err := db.SelectContext(ctx, &rawTables, query, schema); err != nil {
		return nil, false, fmt.Errorf("Error querying information_schema.tables for schema %s: %s", schema, err)
	}
	if len(rawTables) == 0 {
		return []*Table{}, false, nil
	}
	tables := make([]*Table, len(rawTables))
	var havePartitions bool
	for n, rawTable := range rawTables {
		// Note that we no longer set Table.NextAutoIncrement here. information_schema
		// potentially has bad data, e.g. a table without an auto-inc col can still
		// have a non-NULL tables.auto_increment if the original CREATE specified one.
		// Instead the value is parsed from SHOW CREATE TABLE in querySchemaTables().
		tables[n] = &Table{
			Name:               rawTable.Name,
			Engine:             rawTable.Engine.String,
			CharSet:            rawTable.CharSet,
			Collation:          rawTable.TableCollation.String,
			CollationIsDefault: rawTable.CollationIsDefault != "",
			Comment:            rawTable.Comment.String,
		}
		if rawTable.CreateOptions.Valid && rawTable.CreateOptions.String != "" {
			if strings.Contains(strings.ToUpper(rawTable.CreateOptions.String), "PARTITIONED") {
				havePartitions = true
			}
			tables[n].CreateOptions = reformatCreateOptions(rawTable.CreateOptions.String)
		}
	}
	return tables, havePartitions, nil
}

func queryColumnsInSchema(ctx context.Context, db *sqlx.DB, schema string, flavor Flavor) (map[string][]*Column, error) {
	stripDisplayWidth := flavor.OmitIntDisplayWidth()
	var rawColumns []struct {
		Name               string         `db:"COLUMN_NAME"`
		TableName          string         `db:"TABLE_NAME"`
		Type               string         `db:"COLUMN_TYPE"`
		IsNullable         string         `db:"IS_NULLABLE"`
		Default            sql.NullString `db:"COLUMN_DEFAULT"`
		Extra              string         `db:"EXTRA"`
		GenerationExpr     sql.NullString `db:"GENERATION_EXPRESSION"`
		Comment            string         `db:"COLUMN_COMMENT"`
		CharSet            sql.NullString `db:"CHARACTER_SET_NAME"`
		Collation          sql.NullString `db:"COLLATION_NAME"`
		CollationIsDefault sql.NullString `db:"IS_DEFAULT"`
	}

	var query string

	if flavor.Vendor == VendorSnowflake {
		query = `SELECT    
		          c.table_name AS TABLE_NAME, 
		          c.column_name AS COLUMN_NAME,
		          c.data_type AS COLUMN_TYPE, 
		          c.is_nullable AS IS_NULLABLE,
		          coalesce(c.column_default, '') AS COLUMN_DEFAULT, 
		          case when c.is_identity = 'YES' then 'auto_increment' else '' end AS EXTRA,
		          coalesce(c.comment, '') AS COLUMN_COMMENT,
		          coalesce(c.character_set_name, 'utf8') AS CHARACTER_SET_NAME,
		          coalesce(c.collation_name, 'en') AS COLLATION_NAME, 
		          true AS is_default
		FROM      information_schema.columns c
		WHERE     c.table_schema = ?
		ORDER BY  c.table_name, c.ordinal_position;`
	} else {
		query = `
		SELECT    SQL_BUFFER_RESULT
		          c.table_name AS TABLE_NAME, 
		          c.column_name AS COLUMN_NAME,
		          c.column_type AS COLUMN_TYPE, 
		          c.is_nullable AS IS_NULLABLE,
		          c.column_default AS COLUMN_DEFAULT, 
		          c.extra AS EXTRA,
		          %s AS GENERATION_EXPRESSION,
		          c.column_comment AS COLUMN_COMMENT,
		          c.character_set_name AS CHARACTER_SET_NAME,
		          c.collation_name AS COLLATION_NAME, 
		    co.is_default AS is_default
		FROM      information_schema.columns c
		LEFT JOIN information_schema.collations co ON co.collation_name = c.collation_name
		WHERE     c.table_schema = ?
		ORDER BY  c.table_name, c.ordinal_position`

		genExpr := "NULL"
		if flavor.GeneratedColumns() {
			genExpr = "c.generation_expression"
		}
		query = fmt.Sprintf(query, genExpr)
	}

	if err := db.SelectContext(ctx, &rawColumns, query, schema); err != nil {
		return nil, fmt.Errorf("Error querying information_schema.columns for schema %s: %s", schema, err)
	}
	columnsByTableName := make(map[string][]*Column)
	for _, rawColumn := range rawColumns {
		col := &Column{
			Name:          rawColumn.Name,
			TypeInDB:      rawColumn.Type,
			Nullable:      strings.ToUpper(rawColumn.IsNullable) == "YES",
			AutoIncrement: strings.Contains(rawColumn.Extra, "auto_increment"),
			Comment:       rawColumn.Comment,
			Invisible:     strings.Contains(rawColumn.Extra, "INVISIBLE"),
		}
		// If db was upgraded from a pre-8.0.19 version (but still 8.0+) to 8.0.19+,
		// I_S may still contain int display widths even though SHOW CREATE TABLE
		// omits them. Strip to avoid incorrectly flagging the table as unsupported
		// for diffs.
		if stripDisplayWidth {
			col.TypeInDB, _ = StripDisplayWidth(col.TypeInDB) // safe/no-op if already no int display width
		}
		if pos := strings.Index(col.TypeInDB, " /*!100301 COMPRESSED"); pos > -1 {
			// MariaDB includes compression attribute in column type; remove it
			col.Compression = "COMPRESSED"
			col.TypeInDB = col.TypeInDB[0:pos]
		}
		if rawColumn.GenerationExpr.Valid {
			col.GenerationExpr = rawColumn.GenerationExpr.String
			col.Virtual = strings.Contains(rawColumn.Extra, "VIRTUAL GENERATED")
		}
		if !rawColumn.Default.Valid {
			allowNullDefault := col.Nullable && !col.AutoIncrement && col.GenerationExpr == ""
			// Only MariaDB 10.2+ allows blob/text default literals, including explicit
			// DEFAULT NULL clause.
			// Recent versions of MySQL do allow default *expressions* for these col
			// types, but 8.0.13-8.0.22 erroneously omit them from I_S, so we need to
			// catch this situation and parse from SHOW CREATE later.
			if !flavor.Min(FlavorMariaDB102) && (strings.HasSuffix(col.TypeInDB, "blob") || strings.HasSuffix(col.TypeInDB, "text")) {
				allowNullDefault = false
				if strings.Contains(rawColumn.Extra, "DEFAULT_GENERATED") {
					col.Default = "(!!!BLOBDEFAULT!!!)"
				}
			}
			if allowNullDefault {
				col.Default = "NULL"
			}
		} else if flavor.Min(FlavorMariaDB102) {
			if !col.AutoIncrement && col.GenerationExpr == "" {
				// MariaDB 10.2+ exposes defaults as expressions / quote-wrapped strings
				col.Default = rawColumn.Default.String
			}
		} else if strings.HasPrefix(rawColumn.Default.String, "CURRENT_TIMESTAMP") && (strings.HasPrefix(rawColumn.Type, "timestamp") || strings.HasPrefix(rawColumn.Type, "datetime")) {
			col.Default = rawColumn.Default.String
		} else if strings.HasPrefix(rawColumn.Type, "bit") && strings.HasPrefix(rawColumn.Default.String, "b'") {
			col.Default = rawColumn.Default.String
		} else if strings.Contains(rawColumn.Extra, "DEFAULT_GENERATED") {
			// MySQL 8.0.13+ supports default expressions, which are paren-wrapped in
			// SHOW CREATE TABLE in MySQL. However MySQL I_S data has some issues for
			// default expressions. The most common one is fixed here, and if additional
			// mismatches remain, they get corrected by fixDefaultExpression later on.
			col.Default = fmt.Sprintf("(%s)", strings.ReplaceAll(rawColumn.Default.String, "\\'", "'"))
		} else {
			col.Default = fmt.Sprintf("'%s'", EscapeValueForCreateTable(rawColumn.Default.String))
		}
		if matches := reExtraOnUpdate.FindStringSubmatch(rawColumn.Extra); matches != nil {
			col.OnUpdate = matches[1]
			// Some flavors omit fractional precision from ON UPDATE in
			// information_schema only, despite it being present everywhere else
			if openParen := strings.IndexByte(rawColumn.Type, '('); openParen > -1 && !strings.Contains(col.OnUpdate, "(") {
				col.OnUpdate = fmt.Sprintf("%s%s", col.OnUpdate, rawColumn.Type[openParen:])
			}
		}
		if rawColumn.Collation.Valid { // only text-based column types have a notion of charset and collation
			col.CharSet = rawColumn.CharSet.String
			col.Collation = rawColumn.Collation.String
			col.CollationIsDefault = (rawColumn.CollationIsDefault.String != "")
		}
		if columnsByTableName[rawColumn.TableName] == nil {
			columnsByTableName[rawColumn.TableName] = make([]*Column, 0)
		}
		columnsByTableName[rawColumn.TableName] = append(columnsByTableName[rawColumn.TableName], col)
	}
	return columnsByTableName, nil
}

func queryIndexesInSchema(ctx context.Context, db *sqlx.DB, schema string, flavor Flavor) (map[string]*Index, map[string][]*Index, error) {
	if flavor.Vendor == VendorSnowflake {
		// NOTE: Snowflake does not have a statistics table in information_schema. Need to figure out how to gather this
		// information.
		return nil, nil, nil
	}
	var rawIndexes []struct {
		Name       string         `db:"index_name"`
		TableName  string         `db:"table_name"`
		NonUnique  uint8          `db:"non_unique"`
		SeqInIndex uint8          `db:"seq_in_index"`
		ColumnName sql.NullString `db:"column_name"`
		SubPart    sql.NullInt64  `db:"sub_part"`
		Comment    sql.NullString `db:"index_comment"`
		Type       string         `db:"index_type"`
		Collation  sql.NullString `db:"collation"`
		Expression sql.NullString `db:"expression"`
		Visible    string         `db:"is_visible"`
	}
	query := `
		SELECT   SQL_BUFFER_RESULT
		         index_name AS index_name, table_name AS table_name,
		         non_unique AS non_unique, seq_in_index AS seq_in_index,
		         column_name AS column_name, sub_part AS sub_part,
		         index_comment AS index_comment, index_type AS index_type,
		         collation AS collation, %s AS expression, %s AS is_visible
		FROM     information_schema.statistics
		WHERE    table_schema = ?`
	exprSelect, visSelect := "NULL", "'YES'"
	if flavor.Min(FlavorMySQL80) {
		// Index expressions added in 8.0.13
		if flavor.Min(FlavorMySQL80.Dot(13)) {
			exprSelect = "expression"
		}
		visSelect = "is_visible" // available in all 8.0
	} else if flavor.Min(FlavorMariaDB106) {
		// MariaDB I_S uses the inverse: YES for ignored (invisible), NO for visible
		visSelect = "IF(ignored = 'YES', 'NO', 'YES')"
	}
	query = fmt.Sprintf(query, exprSelect, visSelect)
	if err := db.SelectContext(ctx, &rawIndexes, query, schema); err != nil {
		return nil, nil, fmt.Errorf("Error querying information_schema.statistics for schema %s: %s", schema, err)
	}

	primaryKeyByTableName := make(map[string]*Index)
	secondaryIndexesByTableName := make(map[string][]*Index)

	// Since multi-column indexes have multiple rows in the result set, we do two
	// passes over the result: one to figure out which indexes exist, and one to
	// stitch together the col info. We cannot use an ORDER BY on this query, since
	// only the unsorted result matches the same order of secondary indexes as the
	// CREATE TABLE statement.
	indexesByTableAndName := make(map[string]*Index)
	for _, rawIndex := range rawIndexes {
		if rawIndex.SeqInIndex > 1 {
			continue
		}
		index := &Index{
			Name:      rawIndex.Name,
			Unique:    rawIndex.NonUnique == 0,
			Comment:   rawIndex.Comment.String,
			Type:      rawIndex.Type,
			Invisible: (rawIndex.Visible == "NO"),
		}
		if strings.ToUpper(index.Name) == "PRIMARY" {
			primaryKeyByTableName[rawIndex.TableName] = index
			index.PrimaryKey = true
		} else {
			if secondaryIndexesByTableName[rawIndex.TableName] == nil {
				secondaryIndexesByTableName[rawIndex.TableName] = make([]*Index, 0)
			}
			secondaryIndexesByTableName[rawIndex.TableName] = append(secondaryIndexesByTableName[rawIndex.TableName], index)
		}
		fullNameStr := fmt.Sprintf("%s.%s.%s", schema, rawIndex.TableName, rawIndex.Name)
		indexesByTableAndName[fullNameStr] = index
	}
	for _, rawIndex := range rawIndexes {
		fullIndexNameStr := fmt.Sprintf("%s.%s.%s", schema, rawIndex.TableName, rawIndex.Name)
		index, ok := indexesByTableAndName[fullIndexNameStr]
		if !ok {
			panic(fmt.Errorf("Cannot find index %s", fullIndexNameStr))
		}
		for len(index.Parts) < int(rawIndex.SeqInIndex) {
			index.Parts = append(index.Parts, IndexPart{})
		}
		index.Parts[rawIndex.SeqInIndex-1] = IndexPart{
			ColumnName:   rawIndex.ColumnName.String,
			Expression:   rawIndex.Expression.String,
			PrefixLength: uint16(rawIndex.SubPart.Int64),
			Descending:   (rawIndex.Collation.String == "D"),
		}
	}
	return primaryKeyByTableName, secondaryIndexesByTableName, nil
}

func queryForeignKeysInSchema(ctx context.Context, db *sqlx.DB, schema string, flavor Flavor) (map[string][]*ForeignKey, error) {
	var rawForeignKeys []struct {
		Name                 string `db:"constraint_name"`
		TableName            string `db:"table_name"`
		ColumnName           string `db:"column_name"`
		UpdateRule           string `db:"update_rule"`
		DeleteRule           string `db:"delete_rule"`
		ReferencedTableName  string `db:"referenced_table_name"`
		ReferencedSchemaName string `db:"referenced_schema"`
		ReferencedColumnName string `db:"referenced_column_name"`
	}
	var query string
	if flavor.Vendor == VendorSnowflake {
		return nil, nil
		// NOTE: This does not work because the last query is not deterministic and snowflake has no fk column information
		// in the information schema like mysql does.
		// Ref: https://community.snowflake.com/s/question/0D50Z00006w5kwfSAA/how-do-you-get-the-column-names-for-a-foreign-key-constraint
		//
		//query = `
		//SELECT
		//	"fk_name" as "constraint_name"
		//	,"pk_table_name" as "table_name"
		//	,"pk_column_name" as "column_name"
		//	,"update_rule" as "update_rule"
		//	,"delete_rule" as "delete_rule"
		//	,"fk_table_name" as "referenced_table_name"
		//	,"fk_column_name" as "referenced_column_name"
		//	,"fk_schema_name" as "referenced_schema"
		//FROM TABLE(RESULT_SCAN(LAST_QUERY_ID()))
		//WHERE
		//	"pk_schema_name" = 'CORE';`
		//
		//if err := db.SelectContext(ctx, &rawForeignKeys, `SHOW IMPORTED KEYS;`); err != nil {
		//	return nil, fmt.Errorf("Error querying fk relationships %s: %s", schema, err)
		//}
		//
		//if err := db.SelectContext(ctx, &rawForeignKeys, query, schema); err != nil {
		//	return nil, fmt.Errorf("Error querying foreign key constraints for schema %s: %s", schema, err)
		//}
	} else {
		query = `
		SELECT   SQL_BUFFER_RESULT
		         rc.constraint_name AS constraint_name, rc.table_name AS table_name,
		         kcu.column_name AS column_name,
		         rc.update_rule AS update_rule, rc.delete_rule AS delete_rule,
		         rc.referenced_table_name AS referenced_table_name,
		         IF(rc.constraint_schema=rc.unique_constraint_schema, '', rc.unique_constraint_schema) AS referenced_schema,
		         kcu.referenced_column_name AS referenced_column_name
		FROM     information_schema.referential_constraints rc
		JOIN     information_schema.key_column_usage kcu ON kcu.constraint_name = rc.constraint_name AND
		                                 kcu.table_schema = ? AND
		                                 kcu.referenced_column_name IS NOT NULL
		WHERE    rc.constraint_schema = ?
		ORDER BY BINARY rc.constraint_name, kcu.ordinal_position`

		if err := db.SelectContext(ctx, &rawForeignKeys, query, schema, schema); err != nil {
			return nil, fmt.Errorf("Error querying foreign key constraints for schema %s: %s", schema, err)
		}
	}

	foreignKeysByTableName := make(map[string][]*ForeignKey)
	foreignKeysByName := make(map[string]*ForeignKey)
	for _, rawForeignKey := range rawForeignKeys {
		if fk, already := foreignKeysByName[rawForeignKey.Name]; already {
			fk.ColumnNames = append(fk.ColumnNames, rawForeignKey.ColumnName)
			fk.ReferencedColumnNames = append(fk.ReferencedColumnNames, rawForeignKey.ReferencedColumnName)
		} else {
			foreignKey := &ForeignKey{
				Name:                  rawForeignKey.Name,
				ReferencedSchemaName:  rawForeignKey.ReferencedSchemaName,
				ReferencedTableName:   rawForeignKey.ReferencedTableName,
				UpdateRule:            rawForeignKey.UpdateRule,
				DeleteRule:            rawForeignKey.DeleteRule,
				ColumnNames:           []string{rawForeignKey.ColumnName},
				ReferencedColumnNames: []string{rawForeignKey.ReferencedColumnName},
			}
			foreignKeysByName[rawForeignKey.Name] = foreignKey
			foreignKeysByTableName[rawForeignKey.TableName] = append(foreignKeysByTableName[rawForeignKey.TableName], foreignKey)
		}
	}
	return foreignKeysByTableName, nil
}

func queryChecksInSchema(ctx context.Context, db *sqlx.DB, schema string, flavor Flavor) (map[string][]*Check, error) {
	checksByTableName := make(map[string][]*Check)
	var rawChecks []struct {
		Name      string `db:"constraint_name"`
		Clause    string `db:"check_clause"`
		TableName string `db:"table_name"`
		Enforced  string `db:"enforced"`
	}

	// With MariaDB, information_schema.check_constraints has what we need. But
	// nothing in I_S reveals differences between inline-column checks and regular
	// checks, so that is handled separately by parsing SHOW CREATE TABLE later in
	// a fixup function. Also intentionally no ORDER BY in this query; the returned
	// order matches that of SHOW CREATE TABLE (which isn't usually alphabetical).
	//
	// With MySQL, we need to get table names and enforcement status from
	// information_schema.table_constraints. We don't even bother querying
	// information_schema.check_constraints because the clause value there has
	// broken double-escaping logic. Instead we parse bodies from SHOW CREATE
	// TABLE separately in a fixup function.
	var query string
	if flavor.IsMariaDB() {
		query = `
			SELECT   SQL_BUFFER_RESULT
			         constraint_name AS constraint_name, check_clause AS check_clause,
			         table_name AS table_name, 'YES' AS enforced
			FROM     information_schema.check_constraints
			WHERE    constraint_schema = ?`
	} else {
		query = `
			SELECT   SQL_BUFFER_RESULT
			         constraint_name AS constraint_name, '' AS check_clause,
			         table_name AS table_name, enforced AS enforced
			FROM     information_schema.table_constraints
			WHERE    table_schema = ? AND constraint_type = 'CHECK'
			ORDER BY table_name, constraint_name`
	}
	if err := db.SelectContext(ctx, &rawChecks, query, schema); err != nil {
		return nil, fmt.Errorf("Error querying check constraints for schema %s: %s", schema, err)
	}
	for _, rawCheck := range rawChecks {
		check := &Check{
			Name:     rawCheck.Name,
			Clause:   rawCheck.Clause,
			Enforced: strings.ToUpper(rawCheck.Enforced) != "NO",
		}
		checksByTableName[rawCheck.TableName] = append(checksByTableName[rawCheck.TableName], check)
	}
	return checksByTableName, nil
}

func queryPartitionsInSchema(ctx context.Context, db *sqlx.DB, schema string, flavor Flavor) (map[string]*TablePartitioning, error) {
	var rawPartitioning []struct {
		TableName     string         `db:"table_name"`
		PartitionName string         `db:"partition_name"`
		SubName       sql.NullString `db:"subpartition_name"`
		Method        string         `db:"partition_method"`
		SubMethod     sql.NullString `db:"subpartition_method"`
		Expression    sql.NullString `db:"partition_expression"`
		SubExpression sql.NullString `db:"subpartition_expression"`
		Values        sql.NullString `db:"partition_description"`
		Comment       string         `db:"partition_comment"`
	}
	query := `
		SELECT   SQL_BUFFER_RESULT
		         p.table_name AS table_name, p.partition_name AS partition_name,
		         p.subpartition_name AS subpartition_name,
		         p.partition_method AS partition_method,
		         p.subpartition_method AS subpartition_method,
		         p.partition_expression AS partition_expression,
		         p.subpartition_expression AS subpartition_expression,
		         p.partition_description AS partition_description,
		         p.partition_comment AS partition_comment
		FROM     information_schema.partitions p
		WHERE    p.table_schema = ?
		AND      p.partition_name IS NOT NULL
		ORDER BY p.table_name, p.partition_ordinal_position,
		         p.subpartition_ordinal_position`
	if err := db.SelectContext(ctx, &rawPartitioning, query, schema); err != nil {
		return nil, fmt.Errorf("Error querying information_schema.partitions for schema %s: %s", schema, err)
	}

	partitioningByTableName := make(map[string]*TablePartitioning)
	for _, rawPart := range rawPartitioning {
		p, ok := partitioningByTableName[rawPart.TableName]
		if !ok {
			p = &TablePartitioning{
				Method:        rawPart.Method,
				SubMethod:     rawPart.SubMethod.String,
				Expression:    rawPart.Expression.String,
				SubExpression: rawPart.SubExpression.String,
				Partitions:    make([]*Partition, 0),
			}
			partitioningByTableName[rawPart.TableName] = p
		}
		p.Partitions = append(p.Partitions, &Partition{
			Name:    rawPart.PartitionName,
			SubName: rawPart.SubName.String,
			Values:  rawPart.Values.String,
			Comment: rawPart.Comment,
		})
	}
	return partitioningByTableName, nil
}

var reIndexLine = regexp.MustCompile("^\\s+(?:UNIQUE |FULLTEXT |SPATIAL )?KEY `((?:[^`]|``)+)` (?:USING \\w+ )?\\([`(]")

// MySQL 8.0 uses a different index order in SHOW CREATE TABLE than in
// information_schema. This function fixes the struct to match SHOW CREATE
// TABLE's ordering.
func fixIndexOrder(t *Table) {
	byName := t.SecondaryIndexesByName()
	t.SecondaryIndexes = make([]*Index, len(byName))
	var cur int
	for _, line := range strings.Split(t.CreateStatement, "\n") {
		matches := reIndexLine.FindStringSubmatch(line)
		if matches == nil {
			continue
		}
		t.SecondaryIndexes[cur] = byName[matches[1]]
		cur++
	}
	if cur != len(t.SecondaryIndexes) {
		panic(fmt.Errorf("Failed to parse indexes of %s for reordering: only matched %d of %d secondary indexes", t.Name, cur, len(t.SecondaryIndexes)))
	}
}

var reForeignKeyLine = regexp.MustCompile("^\\s+CONSTRAINT `((?:[^`]|``)+)` FOREIGN KEY")

// MySQL 5.5 doesn't alphabetize foreign keys; this function fixes the struct
// to match SHOW CREATE TABLE's order
func fixForeignKeyOrder(t *Table) {
	byName := t.foreignKeysByName()
	t.ForeignKeys = make([]*ForeignKey, len(byName))
	var cur int
	for _, line := range strings.Split(t.CreateStatement, "\n") {
		matches := reForeignKeyLine.FindStringSubmatch(line)
		if matches == nil {
			continue
		}
		t.ForeignKeys[cur] = byName[matches[1]]
		cur++
	}
}

// MySQL 8.0 uses a different order for table options in SHOW CREATE TABLE
// than in information_schema. This function fixes the struct to match SHOW
// CREATE TABLE's ordering.
func fixCreateOptionsOrder(t *Table, flavor Flavor) {
	if !strings.Contains(t.CreateOptions, " ") {
		return
	}

	// Use the generated (but incorrectly-ordered) create statement to build a
	// regexp that pulls out the create options from the actual create string
	genCreate := t.GeneratedCreateStatement(flavor)
	var template string
	for _, line := range strings.Split(genCreate, "\n") {
		if strings.HasPrefix(line, ") ENGINE=") {
			template = line
			break
		}
	}
	template = strings.Replace(template, t.CreateOptions, "!!!CREATEOPTS!!!", 1)
	template = regexp.QuoteMeta(template)
	template = strings.Replace(template, "!!!CREATEOPTS!!!", "(.+)", 1)
	re := regexp.MustCompile(fmt.Sprintf("^%s$", template))

	for _, line := range strings.Split(t.CreateStatement, "\n") {
		if strings.HasPrefix(line, ") ENGINE=") {
			matches := re.FindStringSubmatch(line)
			if matches != nil {
				t.CreateOptions = matches[1]
				return
			}
		}
	}
}

// fixShowCharSets parses SHOW CREATE TABLE to set ForceShowCharSet and
// ForceShowCollation for columns when needed in MySQL 8:
//
// Prior to MySQL 8, the logic behind inclusion of column-level CHARACTER SET
// and COLLATE clauses in SHOW CREATE TABLE was weird but straightforward:
// CHARACTER SET was included whenever the col's *collation* differed from the
// table's default; COLLATION was included whenever the col's collation differed
// from the default collation *of the col's charset*.
//
// MySQL 8 includes these clauses unnecessarily in additional situations:
//   - 8.0 includes column-level character sets and collations whenever specified
//     explicitly in the original CREATE, even when equal to the table's defaults
//   - Tables upgraded from pre-8.0 may omit COLLATE if it's the default for the
//     charset, while tables created in 8.0 will generally include it whenever a
//     CHARACTER SET is shown in a column definition
func fixShowCharSets(t *Table) {
	lines := strings.Split(t.CreateStatement, "\n")
	for n, col := range t.Columns {
		if col.CharSet == "" || col.Collation == "" {
			continue // non-character-based column type, nothing to do
		}
		line := lines[n+1] // columns start on second line of CREATE TABLE
		if col.Collation == t.Collation && strings.Contains(line, "CHARACTER SET "+col.CharSet) {
			col.ForceShowCharSet = true
		}
		if col.CollationIsDefault && strings.Contains(line, "COLLATE "+col.Collation) {
			col.ForceShowCollation = true
		}
	}
}

// MySQL 5.7+ supports generated columns, but mangles them in I_S in various
// ways:
//   - 4-byte characters are not returned properly in I_S since it uses utf8mb3
//   - MySQL 8 incorrectly mangles escaping of single quotes in the I_S value
//   - MySQL 8 potentially uses different charsets introducers for string literals
//     in I_S vs SHOW CREATE
//
// This method modifies each generated Column.GenerationExpr to match SHOW
// CREATE's version.
func fixGenerationExpr(t *Table, flavor Flavor) {
	for _, col := range t.Columns {
		if col.GenerationExpr == "" {
			continue
		}
		if colDefinition := col.Definition(flavor, t); !strings.Contains(t.CreateStatement, colDefinition) {
			var genKind string
			if col.Virtual {
				genKind = "VIRTUAL"
			} else {
				genKind = "STORED"
			}
			reTemplate := `(?m)^\s*` + regexp.QuoteMeta(EscapeIdentifier(col.Name)) + `.+GENERATED ALWAYS AS \((.+)\) ` + genKind
			re := regexp.MustCompile(reTemplate)
			if matches := re.FindStringSubmatch(t.CreateStatement); matches != nil {
				col.GenerationExpr = matches[1]
			}
		}
	}
}

// fixPartitioningEdgeCases handles situations that are reflected in SHOW CREATE
// TABLE, but missing (or difficult to obtain) in information_schema.
func fixPartitioningEdgeCases(t *Table, flavor Flavor) {
	// Handle edge cases for how partitions are expressed in HASH or KEY methods:
	// typically this will just be a PARTITIONS N clause, but it could also be
	// nothing at all, or an explicit list of partitions, depending on how the
	// partitioning was originally created.
	if strings.HasSuffix(t.Partitioning.Method, "HASH") || strings.HasSuffix(t.Partitioning.Method, "KEY") {
		countClause := fmt.Sprintf("\nPARTITIONS %d", len(t.Partitioning.Partitions))
		if strings.Contains(t.CreateStatement, countClause) {
			t.Partitioning.ForcePartitionList = PartitionListCount
		} else if strings.Contains(t.CreateStatement, "\n(PARTITION ") {
			t.Partitioning.ForcePartitionList = PartitionListExplicit
		} else if len(t.Partitioning.Partitions) == 1 {
			t.Partitioning.ForcePartitionList = PartitionListNone
		}
	}

	// KEY methods support an optional ALGORITHM clause, which is present in SHOW
	// CREATE TABLE but not anywhere in information_schema
	if strings.HasSuffix(t.Partitioning.Method, "KEY") && strings.Contains(t.CreateStatement, "ALGORITHM") {
		re := regexp.MustCompile(fmt.Sprintf(`PARTITION BY %s ([^(]*)\(`, t.Partitioning.Method))
		if matches := re.FindStringSubmatch(t.CreateStatement); matches != nil {
			t.Partitioning.AlgoClause = matches[1]
		}
	}

	// Process DATA DIRECTORY clauses, which are easier to parse from SHOW CREATE
	// TABLE instead of information_schema.innodb_sys_tablespaces.
	if (t.Partitioning.ForcePartitionList == PartitionListDefault || t.Partitioning.ForcePartitionList == PartitionListExplicit) &&
		strings.Contains(t.CreateStatement, " DATA DIRECTORY = ") {
		for _, p := range t.Partitioning.Partitions {
			name := p.Name
			if flavor.Min(FlavorMariaDB102) {
				name = EscapeIdentifier(name)
			}
			name = regexp.QuoteMeta(name)
			re := regexp.MustCompile(fmt.Sprintf(`PARTITION %s .*DATA DIRECTORY = '((?:\\\\|\\'|''|[^'])*)'`, name))
			if matches := re.FindStringSubmatch(t.CreateStatement); matches != nil {
				p.DataDir = matches[1]
			}
		}
	}
}

var rePerconaColCompressionLine = regexp.MustCompile("^\\s+`((?:[^`]|``)+)` .* /\\*!50633 COLUMN_FORMAT (COMPRESSED[^*]*) \\*/")

// fixPerconaColCompression parses the table's CREATE string in order to
// populate Column.Compression for columns that are using Percona Server's
// column compression feature, which isn't reflected in information_schema.
func fixPerconaColCompression(t *Table) {
	colsByName := t.ColumnsByName()
	for _, line := range strings.Split(t.CreateStatement, "\n") {
		matches := rePerconaColCompressionLine.FindStringSubmatch(line)
		if matches == nil {
			continue
		}
		colsByName[matches[1]].Compression = matches[2]
	}
}

// fixFulltextIndexParsers parses the table's CREATE string in order to
// populate Index.FullTextParser for any fulltext indexes that specify a parser.
func fixFulltextIndexParsers(t *Table, flavor Flavor) {
	for _, idx := range t.SecondaryIndexes {
		if idx.Type == "FULLTEXT" {
			// Obtain properly-formatted index definition without parser clause, and
			// then build a regex from this which captures the parser name.
			template := fmt.Sprintf("%s /*!50100 WITH PARSER ", idx.Definition(flavor))
			template = regexp.QuoteMeta(template)
			template += "`([^`]+)`"
			re := regexp.MustCompile(template)
			matches := re.FindStringSubmatch(t.CreateStatement)
			if matches != nil { // only matches if a parser is specified
				idx.FullTextParser = matches[1]
			}
		}
	}
}

// fixDefaultExpression parses the table's CREATE string in order to correct
// problems in Column.Default for columns using a default expression in MySQL 8:
//   - In MySQL 8.0.13-8.0.22, blob/text cols may have default expressions but
//     these are omitted from I_S due to a bug fixed in MySQL 8.0.23.
//   - 4-byte characters are not returned properly in I_S since it uses utf8mb3
//   - MySQL 8 incorrectly mangles escaping of single quotes in the I_S value
//   - MySQL 8 potentially uses different charsets introducers for string literals
//     in I_S vs SHOW CREATE
//
// It also fixes problems with BINARY / VARBINARY literal constant defaults in
// MySQL 8, as these are also mangled by I_S if a zero byte is present.
func fixDefaultExpression(t *Table, flavor Flavor) {
	for _, col := range t.Columns {
		if col.Default == "" {
			continue
		}
		var matcher string
		if col.Default[0] == '(' {
			matcher = `.+DEFAULT (\(.+\))`
		} else if strings.HasPrefix(col.Default, "'0x") && strings.Contains(col.TypeInDB, "binary") {
			matcher = `.+DEFAULT ('(''|[^'])*')`
		} else {
			continue
		}
		if colDefinition := col.Definition(flavor, t); !strings.Contains(t.CreateStatement, colDefinition) {
			defaultClause := " DEFAULT " + col.Default
			after := colDefinition[strings.Index(colDefinition, defaultClause)+len(defaultClause):]
			reTemplate := `(?m)^\s*` + regexp.QuoteMeta(EscapeIdentifier(col.Name)) + matcher + regexp.QuoteMeta(after)
			re := regexp.MustCompile(reTemplate)
			if matches := re.FindStringSubmatch(t.CreateStatement); matches != nil {
				col.Default = matches[1]
			}
		}
	}
}

// fixIndexExpression parses the table's CREATE string in order to correct
// problems in index expressions (functional indexes) in MySQL 8:
// * 4-byte characters are not returned properly in I_S since it uses utf8mb3
// * MySQL 8 incorrectly mangles escaping of single quotes in the I_S value
func fixIndexExpression(t *Table, flavor Flavor) {
	// Only need to check secondary indexes, since PK can't contain expressions
	for _, idx := range t.SecondaryIndexes {
		if !idx.Functional() {
			continue
		}
		if idxDefinition := idx.Definition(flavor); !strings.Contains(t.CreateStatement, idxDefinition) {
			exprParts := make([]*IndexPart, 0, len(idx.Parts))
			for n := range idx.Parts {
				if idx.Parts[n].Expression != "" {
					idxDefinition = strings.Replace(idxDefinition, idx.Parts[n].Expression, "!!!EXPR!!!", 1)
					exprParts = append(exprParts, &idx.Parts[n])
				}
			}
			// Build a regex which captures just the index expression(s) for this index
			reTemplate := regexp.QuoteMeta(idxDefinition)
			reTemplate = `(?m)^\s*` + strings.ReplaceAll(reTemplate, "!!!EXPR!!!", "(.*)") + `,?$`
			re := regexp.MustCompile(reTemplate)
			matches := re.FindStringSubmatch(t.CreateStatement)
			for n := 1; n < len(matches); n++ {
				exprParts[n-1].Expression = matches[n]
			}
		}
	}
}

// fixChecks handles the problematic information_schema data for check
// constraints, which is faulty in both MySQL and MariaDB but in different ways.
func fixChecks(t *Table, flavor Flavor) {
	// MariaDB handles CHECKs differently when they're defined inline in a column
	// definition: in this case I_S shows them having a name equal to the column
	// name, but cannot be manipulated using this name directly, nor does this
	// prevent explicitly-named checks from also having that same name.
	// MariaDB also truncates the check clause at 64 bytes in I_S, so we must
	// parse longer checks from SHOW CREATE TABLE.
	if flavor.IsMariaDB() {
		colsByName := t.ColumnsByName()
		var keep []*Check
		for _, cc := range t.Checks {
			if len(cc.Clause) == 64 {
				// This regex is designed to match regular checks as well as inline-column
				template := fmt.Sprintf(`%s[^\n]+CHECK \((%s[^\n]*)\),?\n`,
					regexp.QuoteMeta(EscapeIdentifier(cc.Name)),
					regexp.QuoteMeta(cc.Clause))
				re := regexp.MustCompile(template)
				if matches := re.FindStringSubmatch(t.CreateStatement); matches != nil {
					cc.Clause = matches[1]
				}
			}
			if col, ok := colsByName[cc.Name]; ok && !strings.Contains(t.CreateStatement, cc.Definition(flavor)) {
				col.CheckClause = cc.Clause
			} else {
				keep = append(keep, cc)
			}
		}
		t.Checks = keep
		return
	}

	// Meanwhile, MySQL butchers the escaping of special characters in check
	// clauses I_S, so we parse them from SHOW CREATE TABLE instead
	for _, cc := range t.Checks {
		cc.Clause = "!!!CHECKCLAUSE!!!"
		template := cc.Definition(flavor)
		template = regexp.QuoteMeta(template)
		template = fmt.Sprintf("%s,?\n", strings.Replace(template, cc.Clause, "(.+?)", 1))
		re := regexp.MustCompile(template)
		matches := re.FindStringSubmatch(t.CreateStatement)
		if matches != nil {
			cc.Clause = matches[1]
		}
	}
}

func querySchemaRoutines(ctx context.Context, db *sqlx.DB, schema string, flavor Flavor) ([]*Routine, error) {
	// Obtain the routines in the schema
	// We completely exclude routines that the user can call, but not examine --
	// e.g. user has EXECUTE priv but missing other vital privs. In this case
	// routine_definition will be NULL.
	var rawRoutines []struct {
		Name              string         `db:"routine_name"`
		Type              string         `db:"routine_type"`
		Body              sql.NullString `db:"routine_definition"`
		IsDeterministic   string         `db:"is_deterministic"`
		SQLDataAccess     string         `db:"sql_data_access"`
		SecurityType      string         `db:"security_type"`
		SQLMode           string         `db:"sql_mode"`
		Comment           string         `db:"routine_comment"`
		Definer           string         `db:"definer"`
		DatabaseCollation string         `db:"database_collation"`
	}

	var query string

	if flavor.Vendor == VendorSnowflake {
		query = `
		SELECT 
		       f.FUNCTION_NAME AS function_name, 
               'FUNCTION' AS routine_type,
		       f.FUNCTION_DEFINITION AS routine_definition,
               true AS is_deterministic,
               '' AS sql_data_access,
		       '' AS security_type,
		       '' AS sql_mode, 
               f.comment AS routine_comment,
		       '' AS definer, 
               'utf8' AS database_collation
		FROM   information_schema.functions f
		WHERE  f.function_catalog = ? AND f.function_definition IS NOT NULL

        UNION

    	SELECT 
    		       p.procedure_name AS function_name, 
                   'PROCEDURE' AS routine_type,
    		       p.procedure_definition AS routine_definition,
                   true AS is_deterministic,
                   '' AS sql_data_access,
    		       '' AS security_type,
    		       '' AS sql_mode, 
                   p.comment AS routine_comment,
    		       '' AS definer, 
                   'utf8' AS database_collation
    		FROM   information_schema.procedures p
    		WHERE  p.procedure_schema = ? AND p.procedure_definition IS NOT NULL;`
		if err := db.SelectContext(ctx, &rawRoutines, query, schema, schema); err != nil {
			return nil, fmt.Errorf("Error querying information_schema.routines for schema %s: %s", schema, err)
		}
	} else {
		query = `
		SELECT SQL_BUFFER_RESULT
		       r.routine_name AS routine_name, UPPER(r.routine_type) AS routine_type,
		       r.routine_definition AS routine_definition,
		       UPPER(r.is_deterministic) AS is_deterministic,
		       UPPER(r.sql_data_access) AS sql_data_access,
		       UPPER(r.security_type) AS security_type,
		       r.sql_mode AS sql_mode, r.routine_comment AS routine_comment,
		       r.definer AS definer, r.database_collation AS database_collation
		FROM   information_schema.routines r
		WHERE  r.routine_schema = ? AND routine_definition IS NOT NULL`
		if err := db.SelectContext(ctx, &rawRoutines, query, schema); err != nil {
			return nil, fmt.Errorf("Error querying information_schema.routines for schema %s: %s", schema, err)
		}
	}

	if len(rawRoutines) == 0 {
		return []*Routine{}, nil
	}
	routines := make([]*Routine, len(rawRoutines))
	dict := make(map[ObjectKey]*Routine, len(rawRoutines))
	for n, rawRoutine := range rawRoutines {
		routines[n] = &Routine{
			Name:              rawRoutine.Name,
			Type:              ObjectType(strings.ToLower(rawRoutine.Type)),
			Body:              rawRoutine.Body.String, // This contains incorrect formatting conversions; overwritten later
			Definer:           rawRoutine.Definer,
			DatabaseCollation: rawRoutine.DatabaseCollation,
			Comment:           rawRoutine.Comment,
			Deterministic:     rawRoutine.IsDeterministic == "YES",
			SQLDataAccess:     rawRoutine.SQLDataAccess,
			SecurityType:      rawRoutine.SecurityType,
			SQLMode:           rawRoutine.SQLMode,
		}
		if routines[n].Type != ObjectTypeProc && routines[n].Type != ObjectTypeFunc {
			return nil, fmt.Errorf("Unsupported routine type %s found in %s.%s", rawRoutine.Type, schema, rawRoutine.Name)
		}
		key := ObjectKey{Type: routines[n].Type, Name: routines[n].Name}
		dict[key] = routines[n]
	}

	// Obtain param string, return type string, and full create statement:
	// We can't rely only on information_schema, since it doesn't have the param
	// string formatted in the same way as the original CREATE, nor does
	// routines.body handle strings/charsets correctly for re-runnable SQL.
	// In flavors without the new data dictionary, we first try querying mysql.proc
	// to bulk-fetch sufficient info to rebuild the CREATE without needing to run
	// a SHOW CREATE per routine.
	// If mysql.proc doesn't exist or that query fails, we then run a SHOW CREATE
	// per routine, using multiple goroutines for performance reasons.
	var alreadyObtained int
	if !flavor.Min(FlavorMySQL80) {
		var rawRoutineMeta []struct {
			Name      string `db:"name"`
			Type      string `db:"type"`
			Body      string `db:"body"`
			ParamList string `db:"param_list"`
			Returns   string `db:"returns"`
		}
		query := `
			SELECT name, type, body, param_list, returns
			FROM   mysql.proc
			WHERE  db = ?`
		// Errors here are non-fatal. No need to even check; slice will be empty which is fine
		db.SelectContext(ctx, &rawRoutineMeta, query, schema)
		for _, meta := range rawRoutineMeta {
			key := ObjectKey{Type: ObjectType(strings.ToLower(meta.Type)), Name: meta.Name}
			if routine, ok := dict[key]; ok {
				routine.ParamString = strings.Replace(meta.ParamList, "\r\n", "\n", -1)
				routine.ReturnDataType = meta.Returns
				routine.Body = strings.Replace(meta.Body, "\r\n", "\n", -1)
				routine.CreateStatement = routine.Definition(flavor)
				alreadyObtained++
			}
		}
	}

	var err error
	if alreadyObtained < len(routines) {
		g, subCtx := errgroup.WithContext(ctx)
		for n := range routines {
			r := routines[n] // avoid issues with goroutines and loop iterator values
			if r.CreateStatement == "" {
				g.Go(func() (err error) {
					r.CreateStatement, err = showCreateRoutine(subCtx, db, r.Name, r.Type)
					if err == nil {
						r.CreateStatement = strings.Replace(r.CreateStatement, "\r\n", "\n", -1)
						err = r.parseCreateStatement(flavor, schema)
					} else {
						err = fmt.Errorf("Error executing SHOW CREATE %s for %s.%s: %s", r.Type.Caps(), EscapeIdentifier(schema), EscapeIdentifier(r.Name), err)
					}
					return err
				})
			}
		}
		err = g.Wait()
	}

	return routines, err
}

func showCreateRoutine(ctx context.Context, db *sqlx.DB, routine string, ot ObjectType) (create string, err error) {
	query := fmt.Sprintf("SHOW CREATE %s %s", ot.Caps(), EscapeIdentifier(routine))
	if ot == ObjectTypeProc {
		var createRows []struct {
			CreateStatement sql.NullString `db:"Create Procedure"`
		}
		err = db.SelectContext(ctx, &createRows, query)
		if (err == nil && len(createRows) != 1) || IsDatabaseError(err, mysqlerr.ER_SP_DOES_NOT_EXIST) {
			err = sql.ErrNoRows
		} else if err == nil {
			create = createRows[0].CreateStatement.String
		}
	} else if ot == ObjectTypeFunc {
		var createRows []struct {
			CreateStatement sql.NullString `db:"Create Function"`
		}
		err = db.SelectContext(ctx, &createRows, query)
		if (err == nil && len(createRows) != 1) || IsDatabaseError(err, mysqlerr.ER_SP_DOES_NOT_EXIST) {
			err = sql.ErrNoRows
		} else if err == nil {
			create = createRows[0].CreateStatement.String
		}
	} else {
		err = fmt.Errorf("Object type %s is not a routine", ot)
	}
	return
}
