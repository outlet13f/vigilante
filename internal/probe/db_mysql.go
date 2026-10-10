//go:build !minimal

package probe

// The MySQL driver is not in the minimal build (-tags minimal); PostgreSQL
// (pgx) is always there because the state store needs it.
import _ "github.com/go-sql-driver/mysql"
