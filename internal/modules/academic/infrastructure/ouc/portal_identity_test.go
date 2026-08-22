package ouc

import (
	"errors"
	"testing"
)

func TestParsePortalIdentity(t *testing.T) {
	tests := []struct {
		name         string
		body         string
		expectedNo   string
		want         portalIdentity
		wantMismatch bool
		wantAnyError bool
	}{
		{
			name:       "complete alumni identity with ignored private fields",
			expectedNo: "20260001",
			body: `{
				"e":0,
				"m":"操作成功",
				"d":{"info":{"uid":7,"name":" 海大同学 ","xgh":"20260001","identity":"校友","mobile":"13800000000","email":"student@example.com","avatar":"https://my.ouc.edu.cn/avatar.png"}}
			}`,
			want: portalIdentity{
				RealName:  "海大同学",
				StudentNo: "20260001",
				Identity:  "校友",
			},
		},
		{
			name:         "student number mismatch",
			expectedNo:   "20260001",
			body:         `{"e":0,"d":{"info":{"name":"海大同学","xgh":"20260002","identity":"学生"}}}`,
			wantMismatch: true,
		},
		{
			name:         "nonzero result code",
			expectedNo:   "20260001",
			body:         `{"e":1,"m":"操作失败","d":{"info":{"name":"海大同学","xgh":"20260001","identity":"学生"}}}`,
			wantAnyError: true,
		},
		{
			name:         "missing result code",
			expectedNo:   "20260001",
			body:         `{"d":{"info":{"name":"海大同学","xgh":"20260001","identity":"学生"}}}`,
			wantAnyError: true,
		},
		{
			name:         "missing info",
			expectedNo:   "20260001",
			body:         `{"e":0,"d":{}}`,
			wantAnyError: true,
		},
		{
			name:         "empty name",
			expectedNo:   "20260001",
			body:         `{"e":0,"d":{"info":{"name":" ","xgh":"20260001","identity":"学生"}}}`,
			wantAnyError: true,
		},
		{
			name:         "empty identity",
			expectedNo:   "20260001",
			body:         `{"e":0,"d":{"info":{"name":"海大同学","xgh":"20260001","identity":" "}}}`,
			wantAnyError: true,
		},
		{
			name:         "invalid JSON",
			expectedNo:   "20260001",
			body:         `{"e":0`,
			wantAnyError: true,
		},
		{
			name:         "trailing JSON value",
			expectedNo:   "20260001",
			body:         `{"e":0,"d":{"info":{"name":"海大同学","xgh":"20260001","identity":"学生"}}} {}`,
			wantAnyError: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parsePortalIdentity([]byte(test.body), test.expectedNo)
			if test.wantMismatch {
				if !errors.Is(err, errPortalStudentMismatch) {
					t.Fatalf("error=%v want=%v", err, errPortalStudentMismatch)
				}
				return
			}
			if test.wantAnyError {
				if err == nil {
					t.Fatalf("identity=%+v want error", got)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("identity=%+v error=%v want=%+v", got, err, test.want)
			}
		})
	}
}
