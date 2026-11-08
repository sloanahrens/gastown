package cmd

// The cloud step's Postgres source opens database/sql's "pgx" driver, and
// cloudledger registers no driver itself: the program that wires the source
// imports it, which is this one. Without this import every Postgres read in
// the gt binary fails closed with "unknown driver" (gt-gg9hv.72).
import _ "github.com/jackc/pgx/v5/stdlib"
