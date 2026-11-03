package project

import (
	"errors"
	"math/big"
	"regexp"
	"strings"

	"morde/server/internal/domain"
)

const MicrosPerCurrencyUnit int64 = 1_000_000

type Budget struct {
	Currency    string
	LimitMicros int64
	Version     int64
}

type Usage struct {
	InputTokens  int64
	OutputTokens int64
}

type RateSnapshot struct {
	Provider               string
	ModelID                string
	Model                  string
	Currency               string
	Version                int64
	InputMicrosPerMillion  int64
	OutputMicrosPerMillion int64
}

func (b Budget) Validate() error {
	if !validCurrency(b.Currency) || b.LimitMicros < 1 || b.Version < 1 {
		return errors.New("project budget is invalid")
	}
	return nil
}

func (r RateSnapshot) Validate() error {
	if strings.TrimSpace(r.Provider) != r.Provider || r.Provider == "" || len(r.Provider) > 128 ||
		!domain.ValidModelConnectionID(r.ModelID) || strings.TrimSpace(r.Model) != r.Model || r.Model == "" || len(r.Model) > 256 ||
		!validCurrency(r.Currency) || r.Version < 1 || r.InputMicrosPerMillion < 0 || r.OutputMicrosPerMillion < 0 ||
		r.InputMicrosPerMillion == 0 && r.OutputMicrosPerMillion == 0 {
		return errors.New("project usage rate snapshot is invalid")
	}
	return nil
}

// EstimateTokenCost rounds up to a micro-unit so rounding cannot undercharge.
func (r RateSnapshot) EstimateTokenCost(usage Usage) (int64, error) {
	if err := r.Validate(); err != nil || usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.InputTokens == 0 && usage.OutputTokens == 0 {
		return 0, errors.New("project token usage is invalid")
	}
	input := roundedTokenCost(usage.InputTokens, r.InputMicrosPerMillion)
	output := roundedTokenCost(usage.OutputTokens, r.OutputMicrosPerMillion)
	total := new(big.Int).Add(input, output)
	if !total.IsInt64() {
		return 0, errors.New("project token cost exceeds the supported amount")
	}
	return total.Int64(), nil
}

func roundedTokenCost(tokens, rate int64) *big.Int {
	product := new(big.Int).Mul(big.NewInt(tokens), big.NewInt(rate))
	product.Add(product, big.NewInt(MicrosPerCurrencyUnit-1))
	return product.Div(product, big.NewInt(MicrosPerCurrencyUnit))
}

var currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)

func validCurrency(value string) bool { return currencyPattern.MatchString(value) }
