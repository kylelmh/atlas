// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package mysql

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"ariga.io/atlas/sql/internal/sqlx"
	"ariga.io/atlas/sql/migrate"
	"ariga.io/atlas/sql/schema"
)

// A planApply provides migration capabilities for schema elements.
type planApply struct{ conn }

// PlanChanges returns a migration plan for the given schema changes.
func (p *planApply) PlanChanges(_ context.Context, name string, changes []schema.Change) (*migrate.Plan, error) {
	s := &state{
		conn: p.conn,
		Plan: migrate.Plan{
			Name: name,
			// A plan is reversible, if all
			// its changes are reversible.
			Reversible: true,
			// All statements generated by state will cause implicit commit.
			// https://dev.mysql.com/doc/refman/8.0/en/implicit-commit.html
			Transactional: false,
		},
	}
	if err := s.plan(changes); err != nil {
		return nil, err
	}
	for _, c := range s.Changes {
		if c.Reverse == "" {
			s.Reversible = false
		}
	}
	return &s.Plan, nil
}

// ApplyChanges applies the changes on the database. An error is returned
// if the driver is unable to produce a plan to it, or one of the statements
// is failed or unsupported.
func (p *planApply) ApplyChanges(ctx context.Context, changes []schema.Change) error {
	return sqlx.ApplyChanges(ctx, changes, p)
}

// state represents the state of a planning. It is not part of
// planApply so that multiple planning/applying can be called
// in parallel.
type state struct {
	conn
	migrate.Plan
}

// plan builds the migration plan for applying the
// given changes on the attached connection.
func (s *state) plan(changes []schema.Change) error {
	planned := s.topLevel(changes)
	planned, err := sqlx.DetachCycles(planned)
	if err != nil {
		return err
	}
	for _, c := range planned {
		switch c := c.(type) {
		case *schema.AddTable:
			if err := s.addTable(c); err != nil {
				return err
			}
		case *schema.DropTable:
			s.dropTable(c)
		case *schema.ModifyTable:
			if err := s.modifyTable(c); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported change %T", c)
		}
	}
	return nil
}

// topLevel appends first the changes for creating or dropping schemas (top-level schema elements).
func (s *state) topLevel(changes []schema.Change) []schema.Change {
	planned := make([]schema.Change, 0, len(changes))
	for _, c := range changes {
		switch c := c.(type) {
		case *schema.AddSchema:
			b := Build("CREATE DATABASE").Ident(c.S.Name)
			if sqlx.Has(c.Extra, &schema.IfNotExists{}) {
				b.P("IF NOT EXISTS")
			}
			if a := (schema.Charset{}); sqlx.Has(c.S.Attrs, &a) {
				b.P("CHARACTER SET", a.V)
			}
			if a := (schema.Collation{}); sqlx.Has(c.S.Attrs, &a) {
				b.P("COLLATE", a.V)
			}
			s.append(&migrate.Change{
				Cmd:     b.String(),
				Source:  c,
				Reverse: Build("DROP DATABASE").Ident(c.S.Name).String(),
				Comment: fmt.Sprintf("add new schema named %q", c.S.Name),
			})
		case *schema.DropSchema:
			b := Build("DROP DATABASE").Ident(c.S.Name)
			if sqlx.Has(c.Extra, &schema.IfExists{}) {
				b.P("IF EXISTS")
			}
			s.append(&migrate.Change{
				Cmd:     b.String(),
				Source:  c,
				Comment: fmt.Sprintf("drop schema named %q", c.S.Name),
			})
		default:
			planned = append(planned, c)
		}
	}
	return planned
}

// addTable builds and appends the migrate.Change
// for creating a table in a schema.
func (s *state) addTable(add *schema.AddTable) error {
	b := Build("CREATE TABLE").Table(add.T)
	if sqlx.Has(add.Extra, &schema.IfNotExists{}) {
		b.P("IF NOT EXISTS")
	}
	b.Wrap(func(b *sqlx.Builder) {
		b.MapComma(add.T.Columns, func(i int, b *sqlx.Builder) {
			s.column(b, add.T, add.T.Columns[i])
		})
		if pk := add.T.PrimaryKey; pk != nil {
			b.Comma().P("PRIMARY KEY")
			s.indexParts(b, pk.Parts)
			s.attr(b, pk.Attrs...)
		}
		if len(add.T.Indexes) > 0 {
			b.Comma()
		}
		b.MapComma(add.T.Indexes, func(i int, b *sqlx.Builder) {
			idx := add.T.Indexes[i]
			if idx.Unique {
				b.P("UNIQUE")
			}
			b.P("INDEX").Ident(idx.Name)
			s.indexParts(b, idx.Parts)
			s.attr(b, idx.Attrs...)
		})
		if len(add.T.ForeignKeys) > 0 {
			b.Comma()
			s.fks(b, add.T.ForeignKeys...)
		}
	})
	if err := s.tableAttr(b, add.T.Attrs...); err != nil {
		return err
	}
	s.append(&migrate.Change{
		Cmd:     b.String(),
		Source:  add,
		Reverse: Build("DROP TABLE").Table(add.T).String(),
		Comment: fmt.Sprintf("create %q table", add.T.Name),
	})
	return nil
}

// dropTable builds and appends the migrate.Change
// for dropping a table from a schema.
func (s *state) dropTable(drop *schema.DropTable) {
	b := Build("DROP TABLE").Table(drop.T)
	if sqlx.Has(drop.Extra, &schema.IfExists{}) {
		b.P("IF EXISTS")
	}
	s.append(&migrate.Change{
		Cmd:     b.String(),
		Source:  drop,
		Comment: fmt.Sprintf("drop %q table", drop.T.Name),
	})
}

// modifyTable builds and appends the migrate.Changes for bringing
// the table into its modified state.
func (s *state) modifyTable(modify *schema.ModifyTable) error {
	var changes [2][]schema.Change
	for _, change := range skipAutoChanges(modify.Changes) {
		switch change := change.(type) {
		// Constraints should be dropped before dropping columns, because if a column
		// is a part of multi-column constraints (like, unique index), ALTER TABLE
		// might fail if the intermediate state violates the constraints.
		case *schema.DropIndex:
			changes[0] = append(changes[0], change)
		case *schema.ModifyForeignKey:
			// Foreign-key modification is translated into 2 steps.
			// Dropping the current foreign key and creating a new one.
			changes[0] = append(changes[0], &schema.DropForeignKey{
				F: change.From,
			})
			// Drop the auto-created index for referenced if the reference was changed.
			if change.Change.Is(schema.ChangeRefTable | schema.ChangeRefColumn) {
				changes[0] = append(changes[0], &schema.DropIndex{
					I: &schema.Index{
						Name:  change.From.Symbol,
						Table: modify.T,
					},
				})
			}
			changes[1] = append(changes[1], &schema.AddForeignKey{
				F: change.To,
			})
		// Index modification requires rebuilding the index.
		case *schema.ModifyIndex:
			changes[0] = append(changes[0], &schema.DropIndex{
				I: change.From,
			})
			changes[1] = append(changes[1], &schema.AddIndex{
				I: change.To,
			})
		case *schema.DropAttr:
			return fmt.Errorf("unsupported change type: %T", change)
		default:
			changes[1] = append(changes[1], change)
		}
	}
	for i := range changes {
		if len(changes[i]) > 0 {
			if err := s.alterTable(modify.T, changes[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

// alterTable modifies the given table by executing on it a list of
// changes in one SQL statement.
func (s *state) alterTable(t *schema.Table, changes []schema.Change) error {
	var (
		errors     []string
		b          = Build("ALTER TABLE").Table(t)
		reversible = true
		reverse    = b.Clone()
	)
	b.MapComma(changes, func(i int, b *sqlx.Builder) {
		switch change := changes[i].(type) {
		case *schema.AddColumn:
			b.P("ADD COLUMN")
			s.column(b, t, change.C)
			reverse.P("DROP COLUMN").Ident(change.C.Name)
		case *schema.ModifyColumn:
			b.P("MODIFY COLUMN")
			s.column(b, t, change.To)
			reverse.P("MODIFY COLUMN")
			s.column(reverse, t, change.From)
		case *schema.DropColumn:
			b.P("DROP COLUMN").Ident(change.C.Name)
			reversible = false
		case *schema.AddIndex:
			b.P("ADD")
			if change.I.Unique {
				b.P("UNIQUE")
			}
			b.P("INDEX").Ident(change.I.Name)
			s.indexParts(b, change.I.Parts)
			s.attr(b, change.I.Attrs...)
			reverse.P("DROP INDEX").Ident(change.I.Name)
		case *schema.DropIndex:
			b.P("DROP INDEX").Ident(change.I.Name)
			reversible = false
		case *schema.AddForeignKey:
			b.P("ADD")
			s.fks(b, change.F)
			reverse.P("DROP FOREIGN KEY").Ident(change.F.Symbol)
		case *schema.DropForeignKey:
			b.P("DROP FOREIGN KEY").Ident(change.F.Symbol)
			reversible = false
		case *schema.AddAttr:
			if err := s.tableAttr(b, change.A); err != nil {
				errors = append(errors, fmt.Sprintf("add attribute: %s", err.Error()))
			}
			// Unsupported reverse operation.
			reversible = false
		case *schema.ModifyAttr:
			if err := s.tableAttr(b, change.To); err != nil {
				errors = append(errors, fmt.Sprintf("modify attribute: %s", err.Error()))
			}
			if err := s.tableAttr(reverse, change.From); err != nil {
				errors = append(errors, fmt.Sprintf("reverse modify attribute: %s", err.Error()))
			}
		}
	})
	if len(errors) > 0 {
		return fmt.Errorf("alter table: %s", strings.Join(errors, ", "))
	}
	change := &migrate.Change{
		Cmd: b.String(),
		Source: &schema.ModifyTable{
			T:       t,
			Changes: changes,
		},
		Comment: fmt.Sprintf("modify %q table", t.Name),
	}
	if reversible {
		change.Reverse = reverse.String()
	}
	s.append(change)
	return nil
}

func (s *state) column(b *sqlx.Builder, t *schema.Table, c *schema.Column) {
	b.Ident(c.Name).P(mustFormat(c.Type.Type))
	if !c.Type.Null {
		b.P("NOT")
	}
	b.P("NULL")
	s.columnDefault(b, c)
	// Add manually the JSON_VALID constraint for older
	// versions < 10.4.3. See Driver.checks for full info.
	if _, ok := c.Type.Type.(*schema.JSONType); ok && s.mariadb() && s.ltV("10.4.3") && !sqlx.Has(c.Attrs, &Check{}) {
		b.P("CHECK").Wrap(func(b *sqlx.Builder) {
			b.WriteString(fmt.Sprintf("json_valid(`%s`)", c.Name))
		})
	}
	for _, a := range c.Attrs {
		switch a := a.(type) {
		case *schema.Collation:
			// Define the collation explicitly
			// in case it is not the default.
			if s.collation(t) != a.V {
				b.P("COLLATE", a.V)
			}
		case *OnUpdate:
			b.P("ON UPDATE", a.A)
		case *AutoIncrement:
			b.P("AUTO_INCREMENT")
			// Auto increment with value should be configured on table options.
			if a.V != 0 && !sqlx.Has(t.Attrs, &AutoIncrement{}) {
				t.Attrs = append(t.Attrs, a)
			}
		default:
			s.attr(b, a)
		}
	}
}

func (s *state) indexParts(b *sqlx.Builder, parts []*schema.IndexPart) {
	b.Wrap(func(b *sqlx.Builder) {
		b.MapComma(parts, func(i int, b *sqlx.Builder) {
			switch part := parts[i]; {
			case part.C != nil:
				b.Ident(part.C.Name)
			case part.X != nil:
				b.WriteString(part.X.(*schema.RawExpr).X)
			}
			for _, a := range parts[i].Attrs {
				if c, ok := a.(*schema.Collation); ok && c.V == "D" {
					b.P("DESC")
				}
			}
		})
	})
}

func (s *state) fks(b *sqlx.Builder, fks ...*schema.ForeignKey) {
	b.MapComma(fks, func(i int, b *sqlx.Builder) {
		fk := fks[i]
		if fk.Symbol != "" {
			b.P("CONSTRAINT").Ident(fk.Symbol)
		}
		b.P("FOREIGN KEY")
		b.Wrap(func(b *sqlx.Builder) {
			b.MapComma(fk.Columns, func(i int, b *sqlx.Builder) {
				b.Ident(fk.Columns[i].Name)
			})
		})
		b.P("REFERENCES").Table(fk.RefTable)
		b.Wrap(func(b *sqlx.Builder) {
			b.MapComma(fk.RefColumns, func(i int, b *sqlx.Builder) {
				b.Ident(fk.RefColumns[i].Name)
			})
		})
		if fk.OnUpdate != "" {
			b.P("ON UPDATE", string(fk.OnUpdate))
		}
		if fk.OnDelete != "" {
			b.P("ON DELETE", string(fk.OnDelete))
		}
	})
}

// tableAttr writes the given table attribute to the SQL
// statement builder when a table is created or altered.
func (s *state) tableAttr(b *sqlx.Builder, attrs ...schema.Attr) error {
	for _, a := range attrs {
		switch a := a.(type) {
		case *AutoIncrement:
			if a.V == 0 {
				return fmt.Errorf("missing value for table option AUTO_INCREMENT")
			}
			b.P("AUTO_INCREMENT", strconv.FormatInt(a.V, 10))
		case *schema.Charset:
			b.P("CHARACTER SET", a.V)
		case *schema.Collation:
			b.P("COLLATE", a.V)
		case *schema.Comment:
			b.P("COMMENT", quote(a.Text))
		}
	}
	return nil
}

// collation returns the table collation from its attributes
// or from the default defined in the schema or the database.
func (s *state) collation(t *schema.Table) string {
	var c schema.Collation
	if sqlx.Has(t.Attrs, &c) || t.Schema != nil && sqlx.Has(t.Schema.Attrs, &c) {
		return c.V
	}
	return s.collate
}

func (s *state) append(c *migrate.Change) {
	s.Changes = append(s.Changes, c)
}

func (*state) attr(b *sqlx.Builder, attrs ...schema.Attr) {
	for _, a := range attrs {
		switch a := a.(type) {
		case *schema.Collation:
			b.P("COLLATE", a.V)
		case *schema.Comment:
			b.P("COMMENT", quote(a.Text))
		}
	}
}

// columnDefault writes the default value of column to the builder.
func (s *state) columnDefault(b *sqlx.Builder, c *schema.Column) {
	switch x := c.Default.(type) {
	case *schema.Literal:
		v := x.V
		if !hasNumericDefault(c.Type.Type) && !isHex(v) {
			v = quote(v)
		}
		b.P("DEFAULT", v)
	case *schema.RawExpr:
		v := x.X
		// For backwards compatibility, quote raw expressions that are not wrapped
		// with parens for non-numeric column types (i.e. literals).
		switch t := c.Type.Type; {
		case isHex(v), hasNumericDefault(t), strings.HasPrefix(v, "(") && strings.HasSuffix(v, ")"):
		default:
			if _, ok := t.(*schema.TimeType); !ok || !strings.HasPrefix(strings.ToLower(v), currentTS) {
				v = quote(v)
			}
		}
		b.P("DEFAULT", v)
	}
}

// Build instantiates a new builder and writes the given phrase to it.
func Build(phrase string) *sqlx.Builder {
	b := &sqlx.Builder{QuoteChar: '`'}
	return b.P(phrase)
}

// skipAutoChanges filters unnecessary changes that are automatically
// happened by the database when ALTER TABLE is executed.
func skipAutoChanges(changes []schema.Change) []schema.Change {
	dropC := make(map[string]bool)
	for _, c := range changes {
		if c, ok := c.(*schema.DropColumn); ok {
			dropC[c.C.Name] = true
		}
	}
search:
	for i, c := range changes {
		// Simple case for skipping key dropping, if its columns are dropped.
		// https://dev.mysql.com/doc/refman/8.0/en/alter-table.html#alter-table-add-drop-column
		c, ok := c.(*schema.DropIndex)
		if !ok {
			continue
		}
		for _, p := range c.I.Parts {
			if p.C == nil || !dropC[p.C.Name] {
				continue search
			}
		}
		changes = append(changes[:i], changes[i+1:]...)
	}
	return changes
}

func quote(s string) string {
	if sqlx.IsQuoted(s, '"', '\'') {
		return s
	}
	return strconv.Quote(s)
}

func unquote(s string) (string, error) {
	switch {
	case sqlx.IsQuoted(s, '"'):
		return strconv.Unquote(s)
	case sqlx.IsQuoted(s, '\''):
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'"), nil
	default:
		return s, nil
	}
}
