package ouc

import (
	"testing"

	"github.com/LDouble/campus-academic/internal/modules/academic/infrastructure/academicconfig"
	verificationapp "github.com/LDouble/campus-academic/internal/modules/academic_verification/application"
)

func TestAdaptersSelectIndependentSystemConfiguration(t *testing.T) {
	t.Parallel()
	config := academicconfig.OUCConfig{
		Undergraduate: academicconfig.EndpointSet{
			ServiceURL: "https://jwgl2024.ouc.edu.cn/",
		},
		Graduate: academicconfig.EndpointSet{
			ServiceURL: "https://pgs.ouc.edu.cn/allogene/page/home.htm",
		},
	}
	tests := []struct {
		name  string
		level string
		want  string
	}{
		{
			name:  "undergraduate",
			level: verificationapp.EducationUndergraduate,
			want:  "https://jwgl2024.ouc.edu.cn/",
		},
		{
			name:  "graduate",
			level: verificationapp.EducationGraduate,
			want:  "https://pgs.ouc.edu.cn/allogene/page/home.htm",
		},
	}
	adapters := defaultAdapters()
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			adapter, err := adapterFor(adapters, test.level)
			if err != nil {
				t.Fatal(err)
			}
			if got := adapter.Endpoint(config).ServiceURL; got != test.want {
				t.Fatalf("service URL=%q want=%q", got, test.want)
			}
		})
	}
}

func TestAdapterForRejectsUnknownEducationLevel(t *testing.T) {
	t.Parallel()
	if _, err := adapterFor(defaultAdapters(), "unknown"); err == nil {
		t.Fatal("unknown education level was accepted")
	}
}
