package grepsearchmigrations

import _ "embed"

// Up is also the executable migration used by the one-shot migration role.
//
//go:embed 900001_grepsearch.up.sql
var Up string
