package ouc

import (
	"fmt"

	"github.com/LDouble/campus-academic/internal/modules/academic/application"
	"github.com/LDouble/campus-academic/internal/modules/academic/domain"
	"github.com/LDouble/campus-academic/internal/modules/academic/infrastructure/academicconfig"
	verificationapp "github.com/LDouble/campus-academic/internal/modules/academic_verification/application"
)

// systemAdapter isolates one school-system contract from the shared SSO and
// application-facing provider.
type systemAdapter interface {
	EducationLevel() string
	Endpoint(academicconfig.OUCConfig) academicconfig.EndpointSet
	ParsePeriods([]byte, string) ([]domain.Period, error)
	ParseCourses([]byte, string, string) (domain.CourseSchedule, error)
	ParseCourseSelectionSchedule([]byte, string, string) (domain.CourseSchedule, error)
	ParseGrades([]byte, string, string) ([]domain.Grade, error)
	ParseExams([]byte, string, string) ([]domain.Exam, error)
	ParseSelections([]byte, string, string) ([]domain.CourseSelection, error)
}

type undergraduateAdapter struct{}

func (undergraduateAdapter) EducationLevel() string {
	return verificationapp.EducationUndergraduate
}

func (undergraduateAdapter) Endpoint(config academicconfig.OUCConfig) academicconfig.EndpointSet {
	return config.Undergraduate
}

func (undergraduateAdapter) ParsePeriods(body []byte, encoding string) ([]domain.Period, error) {
	return parseUndergraduatePeriods(body, encoding)
}

func (undergraduateAdapter) ParseCourses(
	body []byte,
	encoding string,
	periodID string,
) (domain.CourseSchedule, error) {
	return parseUndergraduateCourses(body, encoding, periodID)
}

func (undergraduateAdapter) ParseCourseSelectionSchedule(body []byte, encoding string, periodID string) (domain.CourseSchedule, error) {
	return parseUndergraduateCourseSelectionSchedule(body, encoding, periodID)
}

func (undergraduateAdapter) ParseGrades(
	body []byte,
	encoding string,
	periodID string,
) ([]domain.Grade, error) {
	return parseGrades(body, encoding, periodID)
}

func (undergraduateAdapter) ParseExams(
	body []byte,
	encoding string,
	periodID string,
) ([]domain.Exam, error) {
	return parseExams(body, encoding, periodID)
}

func (undergraduateAdapter) ParseSelections(
	body []byte,
	encoding string,
	periodID string,
) ([]domain.CourseSelection, error) {
	return parseSelections(body, encoding, periodID)
}

type graduateAdapter struct{}

func (graduateAdapter) EducationLevel() string {
	return verificationapp.EducationGraduate
}

func (graduateAdapter) Endpoint(config academicconfig.OUCConfig) academicconfig.EndpointSet {
	return config.Graduate
}

func (graduateAdapter) ParsePeriods(body []byte, encoding string) ([]domain.Period, error) {
	return parseGraduatePeriods(body, encoding)
}

func (graduateAdapter) ParseCourses(
	body []byte,
	encoding string,
	periodID string,
) (domain.CourseSchedule, error) {
	courses, err := parseGraduateCourses(body, encoding, periodID)
	return domain.CourseSchedule{Courses: courses}, err
}

func (graduateAdapter) ParseCourseSelectionSchedule([]byte, string, string) (domain.CourseSchedule, error) {
	return domain.CourseSchedule{}, application.ErrProviderUnavailable
}

func (graduateAdapter) ParseGrades(
	body []byte,
	encoding string,
	periodID string,
) ([]domain.Grade, error) {
	return parseGraduateGrades(body, encoding, periodID)
}

func (graduateAdapter) ParseExams(
	body []byte,
	encoding string,
	periodID string,
) ([]domain.Exam, error) {
	return parseGraduateExams(body, encoding, periodID)
}

func (graduateAdapter) ParseSelections(
	body []byte,
	encoding string,
	periodID string,
) ([]domain.CourseSelection, error) {
	return parseGraduateSelections(body, encoding, periodID)
}

func defaultAdapters() map[string]systemAdapter {
	undergraduate := undergraduateAdapter{}
	graduate := graduateAdapter{}
	return map[string]systemAdapter{
		undergraduate.EducationLevel(): undergraduate,
		graduate.EducationLevel():      graduate,
	}
}

func adapterFor(adapters map[string]systemAdapter, educationLevel string) (systemAdapter, error) {
	adapter, ok := adapters[educationLevel]
	if !ok || adapter == nil {
		return nil, fmt.Errorf("unsupported education level")
	}
	return adapter, nil
}

var _ systemAdapter = undergraduateAdapter{}
var _ systemAdapter = graduateAdapter{}
