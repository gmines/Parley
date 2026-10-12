package identity

import (
	"errors"
	"strings"

	"github.com/trustelem/zxcvbn"
	"github.com/trustelem/zxcvbn/match"
)

// minPasswordScore is the lowest acceptable zxcvbn score (0-4). Score 3 means
// an estimated 10^8 or more guesses: it rejects common passwords, sequences,
// keyboard walks, l33t-speak variants and passwords built from the username,
// while short passphrases ("blue-kettle-drift") pass.
//
// We deliberately don't add composition rules (digits, symbols, ...): per
// NIST SP 800-63B they push people towards "Password1!", which zxcvbn rejects.
const minPasswordScore = 3

var ErrWeakPassword = errors.New("password is too easy to guess")

// WeakPasswordError explains why a password was rejected.
type WeakPasswordError struct {
	Hint string
}

func (e *WeakPasswordError) Error() string {
	return "Password is too easy to guess: " + e.Hint + ". A few unrelated words, like a short phrase, makes a strong password."
}

func (e *WeakPasswordError) Is(target error) bool { return target == ErrWeakPassword }

// checkPasswordStrength rejects guessable passwords. userInputs are values the
// password must not be built from (username, display name, product name).
func checkPasswordStrength(password string, userInputs ...string) error {
	inputs := append([]string{"parley"}, userInputs...)
	r := zxcvbn.PasswordStrength(password, inputs)
	if r.Score >= minPasswordScore {
		return nil
	}
	return &WeakPasswordError{Hint: weaknessHint(r.Sequence)}
}

// weaknessHint names the most telling weakness zxcvbn found.
func weaknessHint(seq []*match.Match) string {
	has := func(pred func(*match.Match) bool) bool {
		for _, m := range seq {
			if pred(m) {
				return true
			}
		}
		return false
	}
	dict := func(name string) func(*match.Match) bool {
		return func(m *match.Match) bool { return m.Pattern == "dictionary" && m.DictionaryName == name }
	}
	pattern := func(p string) func(*match.Match) bool {
		return func(m *match.Match) bool { return m.Pattern == p }
	}

	switch {
	case has(dict("user_inputs")):
		return "it contains your username, display name or the app name"
	case has(dict("passwords")):
		return "it is (or is based on) a commonly used password"
	case has(pattern("sequence")):
		return "it contains a sequence like 1234 or abcd"
	case has(pattern("repeat")):
		return "it repeats characters or patterns"
	case has(pattern("spatial")):
		return "it contains a keyboard pattern like qwerty"
	case has(pattern("date")), has(func(m *match.Match) bool { return m.Pattern == "regex" && strings.Contains(m.RegexName, "year") }):
		return "it contains a date or year"
	case has(pattern("dictionary")):
		return "it is based on a common word or name"
	default:
		return "it is too short"
	}
}
