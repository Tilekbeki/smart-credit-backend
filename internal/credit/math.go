package credit

import (
	"encoding/json"
	"errors"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const maxInputAmount int64 = 1_000_000_000_000

var decimalRate = regexp.MustCompile(`^(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)

var errCalculation = errors.New("invalid financing calculation parameters")

// BusinessAgeMonths counts completed UTC calendar months. An anniversary on a
// missing day falls on the last day of that month (January 31 -> February 28).
func BusinessAgeMonths(registered, asOf time.Time) int {
	registered = registered.UTC()
	asOf = asOf.UTC()
	start := time.Date(registered.Year(), registered.Month(), registered.Day(), 0, 0, 0, 0, time.UTC)
	end := time.Date(asOf.Year(), asOf.Month(), asOf.Day(), 0, 0, 0, 0, time.UTC)
	if end.Before(start) {
		return 0
	}
	months := (end.Year()-start.Year())*12 + int(end.Month()-start.Month())
	lastDay := time.Date(end.Year(), end.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day()
	anniversaryDay := min(start.Day(), lastDay)
	if end.Day() < anniversaryDay {
		months--
	}
	return max(0, months)
}

// MonthlyPayment calculates an exact rational annuity and rounds upward to the
// next whole ruble so a displayed affordable payment never understates cost.
func MonthlyPayment(amount int64, annualRate json.Number, term int) (int64, error) {
	if amount < 0 || amount > maxInputAmount {
		return 0, errCalculation
	}
	coefficient, err := annuityCoefficient(annualRate, term)
	if err != nil {
		return 0, err
	}
	payment := new(big.Rat).Mul(new(big.Rat).SetInt64(amount), coefficient)
	whole, remainder := new(big.Int), new(big.Int)
	whole.QuoRem(payment.Num(), payment.Denom(), remainder)
	if remainder.Sign() > 0 {
		whole.Add(whole, big.NewInt(1))
	}
	if !whole.IsInt64() {
		return 0, errCalculation
	}
	return whole.Int64(), nil
}

// AvailableAmount rounds down the principal supported by a whole-ruble budget,
// then caps it at the product limit. Both operations use exact rational math.
func AvailableAmount(budget int64, annualRate json.Number, term int, maxAmount int64) (int64, error) {
	if budget < 0 || budget > maxInputAmount || maxAmount < 0 || maxAmount > maxInputAmount {
		return 0, errCalculation
	}
	coefficient, err := annuityCoefficient(annualRate, term)
	if err != nil {
		return 0, err
	}
	principal := new(big.Rat).Quo(new(big.Rat).SetInt64(budget), coefficient)
	whole := new(big.Int).Quo(principal.Num(), principal.Denom())
	cap := big.NewInt(maxAmount)
	if whole.Cmp(cap) > 0 {
		return maxAmount, nil
	}
	if !whole.IsInt64() {
		return 0, errCalculation
	}
	return whole.Int64(), nil
}

func annuityCoefficient(annualRate json.Number, term int) (*big.Rat, error) {
	if term < 1 || term > 120 {
		return nil, errCalculation
	}
	rate, err := parseRate(annualRate)
	if err != nil {
		return nil, err
	}
	if rate.Sign() == 0 {
		return new(big.Rat).SetFrac64(1, int64(term)), nil
	}
	monthlyRate := new(big.Rat).Quo(rate, big.NewRat(1200, 1))
	base := new(big.Rat).Add(big.NewRat(1, 1), monthlyRate)
	power := new(big.Rat).SetFrac(
		new(big.Int).Exp(base.Num(), big.NewInt(int64(term)), nil),
		new(big.Int).Exp(base.Denom(), big.NewInt(int64(term)), nil),
	)
	denominator := new(big.Rat).Sub(power, big.NewRat(1, 1))
	return new(big.Rat).Quo(new(big.Rat).Mul(monthlyRate, power), denominator), nil
}

func parseRate(value json.Number) (*big.Rat, error) {
	text := value.String()
	if len(text) > 32 || !decimalRate.MatchString(text) {
		return nil, errCalculation
	}
	// Bound exponents before parsing to avoid allocating unbounded integers for
	// malformed stored product data. Normal decimal PostgreSQL rates also work.
	if index := strings.IndexAny(text, "eE"); index >= 0 {
		exponent, err := strconv.Atoi(text[index+1:])
		if err != nil || exponent < -32 || exponent > 32 {
			return nil, errCalculation
		}
	}
	rate, ok := new(big.Rat).SetString(text)
	if !ok || rate.Sign() < 0 || rate.Cmp(big.NewRat(1000, 1)) > 0 {
		return nil, errCalculation
	}
	return rate, nil
}
