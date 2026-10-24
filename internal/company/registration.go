package company

import (
	"regexp"
	"strings"
	"time"
)

var okvedPattern = regexp.MustCompile(`^[0-9]{2}(\.[0-9]{1,2}){0,2}$`)

type RegistrationCheck struct {
	Code    string
	Passed  bool
	Message string
}

// RegistrationChecks is shared by preliminary checks and final policy so a
// contradictory saved check cannot approve invalid frozen company facts.
func RegistrationChecks(profile Company, registrationStatus string, asOf time.Time) []RegistrationCheck {
	organizationType := profile.OrganizationType
	innLength, registrationLength := 10, 13
	if organizationType == "ИП" {
		innLength, registrationLength = 12, 15
	}
	registered, dateErr := time.Parse(time.DateOnly, profile.RegistrationDate)
	return []RegistrationCheck{
		{"company_name", validRegistrationText(profile.Name), "Отсутствует наименование компании."},
		{"organization_type", organizationType == "ИП" || organizationType == "ООО", "Неизвестная организационная форма."},
		{"inn_format", decimalDigits(profile.INN, innLength), "Некорректный формат ИНН."},
		{"registration_number_format", decimalDigits(profile.RegistrationNumber, registrationLength), "Некорректный формат регистрационного номера."},
		{"registration_date", dateErr == nil && !registered.After(asOf.UTC()), "Отсутствует или некорректна дата регистрации."},
		{"okved_format", okvedPattern.MatchString(profile.OKVED), "Отсутствует или некорректен ОКВЭД."},
		{"director_name", validRegistrationText(profile.DirectorName), "Отсутствуют сведения о руководителе."},
		{"registration_active", registrationStatus == "active", "Демонстрационный регистрационный статус компании не является действующим."},
	}
}

func validRegistrationText(value string) bool {
	return strings.TrimSpace(value) != "" && !strings.ContainsRune(value, '\x00')
}

func decimalDigits(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}
