// Package plan defines the shared query plan IR used by the PromQL, LogQL,
// and TraceQL heads. It is deliberately free of any ClickHouse SQL details:
// the SQL emitter in internal/sqlgen is the only package that renders SQL.
package plan

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
)

// Signal identifies the telemetry signal a plan belongs to.
type Signal string

const (
	SignalMetrics Signal = "metrics"
	SignalLogs    Signal = "logs"
	SignalTraces  Signal = "traces"
)

// Query is a single SELECT plan. All user-controlled values are Params. All
// identifiers are validated against the Schema by the emitter.
type Query struct {
	Signal Signal
	// Table is the physical table name from the schema.
	Table  string
	Select []SelectItem
	Where  Expr
	// GroupBy contains select aliases or table column names.
	GroupBy []string
	Having  Expr
	// OrderBy contains select aliases or table column names.
	OrderBy []Order
	// Limit and Offset are bounded by the emitter. Zero means no clause.
	Limit  int
	Offset int
	// TimeColumn is required when TimeRange is set.
	TimeColumn string
	TimeRange  *TimeRange
}

// TimeRange is always parameterized by the emitter.
type TimeRange struct {
	Start        time.Time
	End          time.Time
	EndInclusive bool
}

// SelectItem is one projected expression, optionally aliased.
type SelectItem struct {
	Expr  Expr
	Alias string
}

// Order is one ORDER BY term.
type Order struct {
	By   string
	Desc bool
}

// Expr is a plan expression. The interface is closed to this package.
type Expr interface {
	exprNode()
}

// Column references a whitelisted table column.
type Column struct {
	Name string
}

func (Column) exprNode() {}

// Param is a user-controlled value. It is rendered as a ClickHouse query
// parameter placeholder, never as a SQL literal.
type Param struct {
	Name  string
	Type  string
	Value any
}

func (Param) exprNode() {}

// Number is an internal numeric constant (for example a duration in seconds).
// It is not for user input; use Param for that.
type Number struct {
	Value float64
	IsInt bool
}

func (Number) exprNode() {}

// Interval is an internal SQL interval constant.
type Interval struct {
	Value int64
	Unit  string
}

func (Interval) exprNode() {}

// Func is a whitelisted ClickHouse scalar function.
type Func struct {
	Name string
	Args []Expr
}

func (Func) exprNode() {}

// Aggregate is a whitelisted ClickHouse aggregate function.
type Aggregate struct {
	Name     string
	Args     []Expr
	Distinct bool
}

func (Aggregate) exprNode() {}

// Binary is a whitelisted binary operator.
type Binary struct {
	Op    string
	Left  Expr
	Right Expr
}

func (Binary) exprNode() {}

// Unary is a whitelisted unary operator.
type Unary struct {
	Op   string
	Expr Expr
}

func (Unary) exprNode() {}

// In tests membership of an expression in a list of expressions.
type In struct {
	Expr   Expr
	Values []Expr
	Negate bool
}

func (In) exprNode() {}

// Between tests an inclusive range.
type Between struct {
	Expr   Expr
	Low    Expr
	High   Expr
	Negate bool
}

func (Between) exprNode() {}

// IsNull tests for NULL.
type IsNull struct {
	Expr   Expr
	Negate bool
}

func (IsNull) exprNode() {}

// MapGet reads a map value by key. Both map and key are expressions; a key is
// normally a String Param so label names can never become SQL.
type MapGet struct {
	Map Expr
	Key Expr
}

func (MapGet) exprNode() {}

// Tuple is a parenthesized expression list.
type Tuple struct {
	Elems []Expr
}

func (Tuple) exprNode() {}

// Parameter type names. They are the ClickHouse types used in {name:Type}
// placeholders.
const (
	TypeString     = "String"
	TypeStrings    = "Array(String)"
	TypeDateTime64 = "DateTime64(9)"
	TypeUInt64     = "UInt64"
	TypeInt64      = "Int64"
	TypeFloat64    = "Float64"
	TypeBool       = "UInt8"
)

var (
	identPattern  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	columnPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*$`)
	paramPattern  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)
)

// ValidIdent reports whether s is a safe database, table, alias, or parameter
// identifier. It does not allow dots; columns use ValidColumnName.
func ValidIdent(s string) bool {
	return identPattern.MatchString(s)
}

// ValidColumnName reports whether s is a safe column name or nested column
// path. Each dot-separated segment must be a valid identifier.
func ValidColumnName(s string) bool {
	return columnPattern.MatchString(s)
}

// ValidParamName reports whether s is a legal, non-reserved parameter name.
func ValidParamName(s string) bool {
	return paramPattern.MatchString(s) && !strings.HasPrefix(s, "__")
}

// String returns a String parameter.
func String(name, value string) Param {
	return Param{Name: name, Type: TypeString, Value: value}
}

// Strings returns an Array(String) parameter.
func Strings(name string, values []string) Param {
	cp := make([]string, len(values))
	copy(cp, values)
	return Param{Name: name, Type: TypeStrings, Value: cp}
}

// Time returns a DateTime64(9) parameter.
func Time(name string, value time.Time) Param {
	return Param{Name: name, Type: TypeDateTime64, Value: value}
}

// Uint returns a UInt64 parameter.
func Uint(name string, value uint64) Param {
	return Param{Name: name, Type: TypeUInt64, Value: value}
}

// Int returns an Int64 parameter.
func Int(name string, value int64) Param {
	return Param{Name: name, Type: TypeInt64, Value: value}
}

// Float returns a Float64 parameter.
func Float(name string, value float64) Param {
	return Param{Name: name, Type: TypeFloat64, Value: value}
}

// Bool returns a UInt8 parameter representing a boolean.
func Bool(name string, value bool) Param {
	return Param{Name: name, Type: TypeBool, Value: value}
}

// Col returns a column expression.
func Col(name string) Column {
	return Column{Name: name}
}

// F returns a scalar function expression.
func F(name string, args ...Expr) Func {
	return Func{Name: name, Args: args}
}

// Agg returns an aggregate function expression.
func Agg(name string, args ...Expr) Aggregate {
	return Aggregate{Name: name, Args: args}
}

// Bin returns a binary expression.
func Bin(op string, left, right Expr) Binary {
	return Binary{Op: op, Left: left, Right: right}
}

// Un returns a unary expression.
func Un(op string, expr Expr) Unary {
	return Unary{Op: op, Expr: expr}
}

// Map returns a map lookup expression.
func Map(m Expr, key Expr) MapGet {
	return MapGet{Map: m, Key: key}
}

// Integer returns an internal integer constant.
func Integer(v int64) Number {
	return Number{Value: float64(v), IsInt: true}
}

// Decimal returns an internal floating-point constant.
func Decimal(v float64) Number {
	return Number{Value: v}
}

// IntInterval returns an internal SQL interval.
func IntInterval(v int64, unit string) Interval {
	return Interval{Value: v, Unit: unit}
}

// ValidateParam checks the name, type, and value of a parameter. Values are
// validated here so the HTTP client can encode them without surprises.
func ValidateParam(p Param) error {
	if !ValidParamName(p.Name) {
		return fmt.Errorf("invalid parameter name %q", p.Name)
	}
	return ValidateParamValue(p)
}

// ValidateParamValue checks the type and value of a parameter without applying
// the user-parameter name rules. The emitter uses it for its reserved
// parameters (__start, __end, __limit, __offset).
func ValidateParamValue(p Param) error {
	switch p.Type {
	case TypeString:
		if _, ok := p.Value.(string); !ok {
			return fmt.Errorf("parameter %q: expected string, got %T", p.Name, p.Value)
		}
	case TypeStrings:
		if _, ok := p.Value.([]string); !ok {
			return fmt.Errorf("parameter %q: expected []string, got %T", p.Name, p.Value)
		}
	case TypeDateTime64:
		if _, ok := p.Value.(time.Time); !ok {
			return fmt.Errorf("parameter %q: expected time.Time, got %T", p.Name, p.Value)
		}
	case TypeUInt64:
		switch v := p.Value.(type) {
		case uint64:
		case uint:
		case int:
			if v < 0 {
				return fmt.Errorf("parameter %q: negative UInt64 value", p.Name)
			}
		case int64:
			if v < 0 {
				return fmt.Errorf("parameter %q: negative UInt64 value", p.Name)
			}
		default:
			return fmt.Errorf("parameter %q: expected unsigned integer, got %T", p.Name, p.Value)
		}
	case TypeInt64:
		switch p.Value.(type) {
		case int64, int:
		default:
			return fmt.Errorf("parameter %q: expected integer, got %T", p.Name, p.Value)
		}
	case TypeFloat64:
		switch v := p.Value.(type) {
		case float64:
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return fmt.Errorf("parameter %q: non-finite float", p.Name)
			}
		case float32:
			if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
				return fmt.Errorf("parameter %q: non-finite float", p.Name)
			}
		case int:
		case int64:
		default:
			return fmt.Errorf("parameter %q: expected float, got %T", p.Name, p.Value)
		}
	case TypeBool:
		if _, ok := p.Value.(bool); !ok {
			return fmt.Errorf("parameter %q: expected bool, got %T", p.Name, p.Value)
		}
	default:
		return fmt.Errorf("parameter %q: unsupported type %q", p.Name, p.Type)
	}
	return nil
}

// ValidateTimeRange checks that a range is present, ordered, and within max.
// A zero max disables the upper bound.
func (r TimeRange) Validate(max time.Duration) error {
	if r.Start.IsZero() || r.End.IsZero() {
		return errors.New("time range start and end are required")
	}
	if !r.Start.Before(r.End) {
		return errors.New("time range start must be before end")
	}
	if max > 0 && r.End.Sub(r.Start) > max {
		return fmt.Errorf("time range exceeds maximum of %s", max)
	}
	return nil
}
