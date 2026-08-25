// Package infrastructure implements persistence and external-source adapters
// for aggregate academic statistics.
package infrastructure

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/LDouble/campus-academic/internal/modules/academic_statistics/domain"
	mysqldriver "github.com/go-sql-driver/mysql"
)

const sourceAggregateQueryTemplate = `
WITH source_rows AS (
    SELECT
        TRIM(COALESCE(term_id, '')) AS term_id,
        TRIM(term_code) AS term_code,
        TRIM(course_code) AS course_code,
        COALESCE(NULLIF(TRIM(course_name), ''), TRIM(course_code)) AS course_name,
        REGEXP_REPLACE(TRIM(COALESCE(teacher_name, '')), '[[:space:]]+', '') AS teacher_name,
        TRIM(COALESCE(selection_id, '')) AS selection_id,
        TRIM(COALESCE(score_str, '')) AS score_str,
        score
    FROM grade_details
    WHERE student_id IS NOT NULL
      AND TRIM(student_id) <> ''
      AND OCTET_LENGTH(student_id) = OCTET_LENGTH(TRIM(student_id))
      AND grade_id IS NOT NULL
      AND TRIM(grade_id) <> ''
      AND OCTET_LENGTH(grade_id) = OCTET_LENGTH(TRIM(grade_id))
      AND deleted_at IS NULL
      AND course_code IS NOT NULL
      AND TRIM(course_code) <> ''
      AND term_code IS NOT NULL
      AND TRIM(term_code) <> ''
),
normalized AS (
    SELECT
        term_id,
        term_code,
        course_code,
        course_name,
        teacher_name,
        selection_id,
        score_str,
        CASE
            WHEN score BETWEEN 0 AND 100 THEN score
            WHEN score IS NULL
             AND score_str REGEXP '^[0-9]+([.][0-9]+)?$'
             AND CAST(score_str AS DECIMAL(5, 2)) BETWEEN 0 AND 100
                THEN CAST(score_str AS DECIMAL(5, 2))
            ELSE NULL
        END AS numeric_score
    FROM source_rows
),
classified AS (
    SELECT
        term_id,
        term_code,
        course_code,
        course_name,
        teacher_name,
        selection_id,
        score_str,
        numeric_score,
        CASE
            WHEN numeric_score IS NOT NULL THEN numeric_score >= 60
            WHEN score_str IN ('优秀', '良好', '中等', '及格', '合格', '通过') THEN 1
            WHEN score_str IN ('不及格', '不合格', '未通过') THEN 0
            ELSE NULL
        END AS passed
    FROM normalized
),
valid AS (
    SELECT *
    FROM classified
    WHERE passed IS NOT NULL
)
SELECT /*+ MAX_EXECUTION_TIME(%d) */ *
FROM (
SELECT
    /*+ NO_MERGE(valid) */
    'course' AS dimension_type,
    term_id,
    term_code,
    course_code,
    MAX(course_name) AS course_name,
    '' AS teacher_name,
    0 AS class_count,
    COUNT(*) AS valid_count,
    SUM(passed = 1) AS pass_count,
    SUM(passed = 0) AS fail_count,
    SUM(numeric_score IS NOT NULL) AS numeric_score_count,
    CAST(COALESCE(SUM(ROUND(numeric_score * 100)), 0) AS SIGNED) AS numeric_score_sum_x100,
    SUM(numeric_score IS NOT NULL AND numeric_score < 60) AS numeric_fail_count,
    SUM(numeric_score >= 60 AND numeric_score < 70) AS score_60_69_count,
    SUM(numeric_score >= 70 AND numeric_score < 80) AS score_70_79_count,
    SUM(numeric_score >= 80 AND numeric_score < 90) AS score_80_89_count,
    SUM(numeric_score >= 90 AND numeric_score <= 100) AS score_90_100_count,
    SUM(numeric_score IS NULL AND score_str = '优秀') AS level_excellent_count,
    SUM(numeric_score IS NULL AND score_str = '良好') AS level_good_count,
    SUM(numeric_score IS NULL AND score_str = '中等') AS level_medium_count,
    SUM(numeric_score IS NULL AND score_str IN ('及格', '合格', '通过')) AS level_pass_count,
    SUM(numeric_score IS NULL AND score_str IN ('不及格', '不合格', '未通过')) AS level_fail_count
FROM valid
GROUP BY term_id, term_code, course_code

UNION ALL

SELECT
    /*+ NO_MERGE(valid) */
    'instructor' AS dimension_type,
    term_id,
    term_code,
    course_code,
    MAX(course_name) AS course_name,
    teacher_name,
    COUNT(DISTINCT selection_id) AS class_count,
    COUNT(*) AS valid_count,
    SUM(passed = 1) AS pass_count,
    SUM(passed = 0) AS fail_count,
    SUM(numeric_score IS NOT NULL) AS numeric_score_count,
    CAST(COALESCE(SUM(ROUND(numeric_score * 100)), 0) AS SIGNED) AS numeric_score_sum_x100,
    SUM(numeric_score IS NOT NULL AND numeric_score < 60) AS numeric_fail_count,
    SUM(numeric_score >= 60 AND numeric_score < 70) AS score_60_69_count,
    SUM(numeric_score >= 70 AND numeric_score < 80) AS score_70_79_count,
    SUM(numeric_score >= 80 AND numeric_score < 90) AS score_80_89_count,
    SUM(numeric_score >= 90 AND numeric_score <= 100) AS score_90_100_count,
    SUM(numeric_score IS NULL AND score_str = '优秀') AS level_excellent_count,
    SUM(numeric_score IS NULL AND score_str = '良好') AS level_good_count,
    SUM(numeric_score IS NULL AND score_str = '中等') AS level_medium_count,
    SUM(numeric_score IS NULL AND score_str IN ('及格', '合格', '通过')) AS level_pass_count,
    SUM(numeric_score IS NULL AND score_str IN ('不及格', '不合格', '未通过')) AS level_fail_count
FROM valid
WHERE teacher_name <> ''
  AND selection_id <> ''
  AND LOCATE(',', teacher_name) = 0
  AND LOCATE('，', teacher_name) = 0
  AND LOCATE('、', teacher_name) = 0
  AND LOCATE(';', teacher_name) = 0
  AND LOCATE('；', teacher_name) = 0
  AND LOCATE('/', teacher_name) = 0
GROUP BY term_id, term_code, course_code, teacher_name
) AS aggregate_rows
`

const sourceCutoffQuery = `
SELECT CAST(
    ROUND(UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3)) * 1000)
    AS SIGNED
)`

const sourceUniquenessQuery = `
SELECT COUNT(*)
FROM (
    SELECT index_name
    FROM information_schema.statistics
    WHERE table_schema = DATABASE()
      AND table_name = 'grade_details'
      AND non_unique = 0
    GROUP BY index_name
    HAVING GROUP_CONCAT(
        column_name
        ORDER BY seq_in_index
        SEPARATOR ','
    ) = 'student_id,grade_id'
) AS matching_unique_indexes`

const sourceTableExistenceQuery = `
SELECT COUNT(*)
FROM information_schema.tables
WHERE table_schema = DATABASE()
  AND table_name = 'grade_details'
  AND table_type = 'BASE TABLE'`

const maxSourceCAFileSize = 1 << 20

// GradeAggregateSource executes the complete aggregation inside the read-only
// source database and returns aggregate rows only.
type GradeAggregateSource struct {
	db            *sql.DB
	timeout       time.Duration
	tlsConfigName string
	allowWrites   bool
}

// PinnedGradeSourceConfig describes the operator-only source connection. Host
// is expected to be a loopback endpoint of the temporary SSH tunnel.
type PinnedGradeSourceConfig struct {
	Host             string
	Port             int
	Database         string
	User             string
	Password         string
	CAFile           string
	ServerCertSHA256 string
	Timeout          time.Duration
}

// NewGradeAggregateSource validates the source DSN and configures a small
// connection pool with an explicit source-grant policy.
func NewGradeAggregateSource(
	dsn string,
	timeout time.Duration,
	allowWrites bool,
) (*GradeAggregateSource, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open grade aggregate source: %w", err)
	}
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(30 * time.Minute)
	return &GradeAggregateSource{
		db:          db,
		timeout:     timeout,
		allowWrites: allowWrites,
	}, nil
}

// NewPinnedGradeAggregateSource creates a TLS source connection whose trust
// decision is both a fixed CA-chain verification and an exact leaf-certificate
// SHA-256 pin. It intentionally leaves hostname verification to the tunnel
// boundary so it can support the legacy certificate that contains no SAN.
// This constructor is reserved for the synchronous operator command.
func NewPinnedGradeAggregateSource(
	config PinnedGradeSourceConfig,
) (*GradeAggregateSource, error) {
	caPEM, err := readPublicRegularFile(config.CAFile, maxSourceCAFileSize)
	if err != nil {
		return nil, fmt.Errorf("read grade source CA: %w", err)
	}
	tlsConfig, err := newPinnedSourceTLSConfig(
		caPEM,
		config.ServerCertSHA256,
	)
	if err != nil {
		return nil, err
	}
	name, err := randomTLSConfigName()
	if err != nil {
		return nil, err
	}
	if err = mysqldriver.RegisterTLSConfig(name, tlsConfig); err != nil {
		return nil, fmt.Errorf("register grade source TLS profile: %w", err)
	}
	driverConfig := mysqldriver.NewConfig()
	driverConfig.User = config.User
	driverConfig.Passwd = config.Password
	driverConfig.Net = "tcp"
	driverConfig.Addr = net.JoinHostPort(
		config.Host,
		fmt.Sprintf("%d", config.Port),
	)
	driverConfig.DBName = config.Database
	driverConfig.ParseTime = true
	driverConfig.Loc = time.UTC
	driverConfig.Timeout = 15 * time.Second
	driverConfig.TLSConfig = name
	driverConfig.Params = map[string]string{"charset": "utf8mb4"}
	db, err := sql.Open("mysql", driverConfig.FormatDSN())
	if err != nil {
		mysqldriver.DeregisterTLSConfig(name)
		return nil, fmt.Errorf("open pinned grade aggregate source: %w", err)
	}
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(30 * time.Minute)
	return &GradeAggregateSource{
		db:            db,
		timeout:       config.Timeout,
		tlsConfigName: name,
	}, nil
}

// Close releases source-database connections.
func (source *GradeAggregateSource) Close() error {
	if source == nil || source.db == nil {
		return nil
	}
	err := source.db.Close()
	if source.tlsConfigName != "" {
		mysqldriver.DeregisterTLSConfig(source.tlsConfigName)
		source.tlsConfigName = ""
	}
	return err
}

// Preflight verifies connectivity, the expected source table/key, and the
// least-privilege account grants without creating a product publication batch.
func (source *GradeAggregateSource) Preflight(parent context.Context) error {
	if source == nil || source.db == nil {
		return errors.New("grade aggregate source is unavailable")
	}
	timeout := source.timeout
	if timeout <= 0 || timeout > 30*time.Second {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	connection, err := source.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("connect to grade source: %w", err)
	}
	defer func() { _ = connection.Close() }()
	if err = connection.PingContext(ctx); err != nil {
		return fmt.Errorf("ping grade source: %w", err)
	}
	var matchingTables int
	if err = connection.QueryRowContext(ctx, sourceTableExistenceQuery).
		Scan(&matchingTables); err != nil {
		return fmt.Errorf("validate grade source table: %w", err)
	}
	if matchingTables != 1 {
		return errors.New("grade source table grade_details is unavailable")
	}
	var matchingUniqueIndexes int
	if err = connection.QueryRowContext(ctx, sourceUniquenessQuery).
		Scan(&matchingUniqueIndexes); err != nil {
		return fmt.Errorf("validate grade source uniqueness: %w", err)
	}
	if matchingUniqueIndexes < 1 {
		return errors.New(
			"grade source must enforce UNIQUE(student_id, grade_id)",
		)
	}
	grantRows, err := connection.QueryContext(ctx, "SHOW GRANTS FOR CURRENT_USER")
	if err != nil {
		return fmt.Errorf("inspect grade source grants: %w", err)
	}
	defer func() { _ = grantRows.Close() }()
	grants := make([]string, 0, 2)
	for grantRows.Next() {
		var grant string
		if err = grantRows.Scan(&grant); err != nil {
			return fmt.Errorf("scan grade source grant: %w", err)
		}
		grants = append(grants, grant)
	}
	if err = grantRows.Err(); err != nil {
		return fmt.Errorf("read grade source grants: %w", err)
	}
	if err = validateSourceGrants(grants, source.allowWrites); err != nil {
		return err
	}
	return nil
}

func validateSourceGrants(grants []string, allowWrites bool) error {
	hasSelect := false
	hasRequiredWrites := false
	for _, grant := range grants {
		normalized := strings.ToUpper(strings.Join(strings.Fields(grant), " "))
		switch {
		case strings.HasPrefix(normalized, "GRANT USAGE ON *.* TO "):
			if strings.Contains(normalized, " WITH GRANT OPTION") {
				return errors.New("grade source account must not grant privileges")
			}
		case strings.HasPrefix(normalized, "GRANT SELECT ON ") && strings.Contains(normalized, " TO "):
			if strings.Contains(normalized, " WITH GRANT OPTION") {
				return errors.New("grade source account must not grant privileges")
			}
			hasSelect = true
		case strings.HasPrefix(normalized, "GRANT SELECT, INSERT, UPDATE ON ") && strings.Contains(normalized, " TO "):
			if !allowWrites {
				return fmt.Errorf("grade source account has a non-read-only grant: %s", redactGrant(grant))
			}
			if strings.Contains(normalized, " WITH GRANT OPTION") {
				return errors.New("grade source account must not grant privileges")
			}
			hasSelect = true
			hasRequiredWrites = true
		default:
			return fmt.Errorf(
				"grade source account has a non-read-only grant: %s",
				redactGrant(grant),
			)
		}
	}
	if !hasSelect {
		return errors.New("grade source account has no SELECT grant")
	}
	if allowWrites && !hasRequiredWrites {
		return errors.New("managed grade source account requires SELECT, INSERT, UPDATE grant")
	}
	return nil
}

func redactGrant(grant string) string {
	normalized := strings.Join(strings.Fields(grant), " ")
	if index := strings.Index(strings.ToUpper(normalized), " TO "); index >= 0 {
		return normalized[:index]
	}
	return "unsupported grant"
}

func newPinnedSourceTLSConfig(
	caPEM []byte,
	fingerprint string,
) (*tls.Config, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("parse grade source CA")
	}
	wantFingerprint, err := parseSHA256Fingerprint(fingerprint)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, // Verification is performed below without SAN matching.
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return errors.New("grade source did not present a certificate")
			}
			leaf := state.PeerCertificates[0]
			intermediates := x509.NewCertPool()
			for _, certificate := range state.PeerCertificates[1:] {
				intermediates.AddCert(certificate)
			}
			if _, verifyErr := leaf.Verify(x509.VerifyOptions{
				Roots:         pool,
				Intermediates: intermediates,
				KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			}); verifyErr != nil {
				return fmt.Errorf("verify grade source certificate chain: %w", verifyErr)
			}
			gotFingerprint := sha256.Sum256(leaf.Raw)
			if subtle.ConstantTimeCompare(
				gotFingerprint[:],
				wantFingerprint,
			) != 1 {
				return errors.New("grade source certificate fingerprint mismatch")
			}
			return nil
		},
	}, nil
}

func parseSHA256Fingerprint(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	if len(value) >= len("SHA256:") &&
		strings.EqualFold(value[:len("SHA256:")], "SHA256:") {
		value = value[len("SHA256:"):]
	}
	value = strings.ReplaceAll(value, ":", "")
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		return nil, errors.New(
			"grade source certificate SHA-256 fingerprint must contain 32 bytes",
		)
	}
	return decoded, nil
}

func randomTLSConfigName() (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate grade source TLS profile name: %w", err)
	}
	return "academic-statistics-pinned-" + hex.EncodeToString(random), nil
}

func readPublicRegularFile(path string, maximum int64) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("file path must be absolute")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("file must be regular and not a symbolic link")
	}
	// #nosec G304 -- path is an explicit operator-controlled read-only mount.
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximum {
		return nil, errors.New("file exceeds safe size")
	}
	if block, _ := pem.Decode(data); block == nil {
		return nil, errors.New("file does not contain a PEM certificate")
	}
	return data, nil
}

// Aggregate computes course and teacher-course statistics in one consistent,
// read-only database transaction.
func (source *GradeAggregateSource) Aggregate(
	parent context.Context,
) (domain.Snapshot, error) {
	if source == nil || source.db == nil {
		return domain.Snapshot{}, errors.New("grade aggregate source is unavailable")
	}
	ctx, cancel := context.WithTimeout(parent, source.timeout)
	defer cancel()
	tx, err := source.db.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})
	if err != nil {
		return domain.Snapshot{}, fmt.Errorf("begin source snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// Capture database time before the first consistent read. The aggregate
	// snapshot may be established slightly later, so this is a conservative
	// freshness boundary that never claims visibility beyond the snapshot.
	var sourceCutoffMillis int64
	if err = tx.QueryRowContext(ctx, sourceCutoffQuery).
		Scan(&sourceCutoffMillis); err != nil {
		return domain.Snapshot{}, fmt.Errorf(
			"read source snapshot cutoff: %w",
			err,
		)
	}
	var matchingUniqueIndexes int
	if err = tx.QueryRowContext(ctx, sourceUniquenessQuery).
		Scan(&matchingUniqueIndexes); err != nil {
		return domain.Snapshot{}, fmt.Errorf(
			"validate grade source uniqueness: %w",
			err,
		)
	}
	if matchingUniqueIndexes < 1 {
		return domain.Snapshot{}, errors.New(
			"grade source must enforce UNIQUE(student_id, grade_id)",
		)
	}
	query := fmt.Sprintf(sourceAggregateQueryTemplate, source.timeout.Milliseconds())
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return domain.Snapshot{}, fmt.Errorf("query source aggregate: %w", err)
	}
	defer func() { _ = rows.Close() }()
	snapshot := domain.Snapshot{
		Courses:     []domain.CourseTermAggregate{},
		Instructors: []domain.InstructorCourseTermAggregate{},
	}
	for rows.Next() {
		row := sourceAggregateRow{}
		if err = row.scan(rows); err != nil {
			return domain.Snapshot{}, err
		}
		identity, ok := normalizeSourceTerm(row.TermID, row.TermCode)
		if !ok {
			continue
		}
		switch row.DimensionType {
		case "course":
			course := row.course(identity)
			snapshot.SourceRowCount += course.ValidCount
			snapshot.Courses = append(snapshot.Courses, course)
		case "instructor":
			instructor, ok := row.instructor(identity)
			if ok {
				snapshot.Instructors = append(snapshot.Instructors, instructor)
			}
		default:
			return domain.Snapshot{}, fmt.Errorf(
				"unexpected source aggregate dimension %q",
				row.DimensionType,
			)
		}
	}
	if err = rows.Err(); err != nil {
		return domain.Snapshot{}, fmt.Errorf("read source aggregate: %w", err)
	}
	if err = rows.Close(); err != nil {
		return domain.Snapshot{}, fmt.Errorf("close source aggregate rows: %w", err)
	}
	snapshot.SourceCutoffAt = time.UnixMilli(sourceCutoffMillis).UTC()
	if err = tx.Commit(); err != nil {
		return domain.Snapshot{}, fmt.Errorf("commit source snapshot: %w", err)
	}
	return snapshot, nil
}

type sourceAggregateRow struct {
	DimensionType       string
	TermID              string
	TermCode            string
	CourseCode          string
	CourseName          string
	TeacherName         string
	ClassCount          int64
	ValidCount          int64
	PassCount           int64
	FailCount           int64
	NumericScoreCount   int64
	NumericScoreSumX100 int64
	Distribution        domain.Distribution
}

func (row *sourceAggregateRow) scan(rows *sql.Rows) error {
	err := rows.Scan(
		&row.DimensionType,
		&row.TermID,
		&row.TermCode,
		&row.CourseCode,
		&row.CourseName,
		&row.TeacherName,
		&row.ClassCount,
		&row.ValidCount,
		&row.PassCount,
		&row.FailCount,
		&row.NumericScoreCount,
		&row.NumericScoreSumX100,
		&row.Distribution.NumericFail,
		&row.Distribution.Score6069,
		&row.Distribution.Score7079,
		&row.Distribution.Score8089,
		&row.Distribution.Score90100,
		&row.Distribution.LevelExcellent,
		&row.Distribution.LevelGood,
		&row.Distribution.LevelMedium,
		&row.Distribution.LevelPass,
		&row.Distribution.LevelFail,
	)
	if err != nil {
		return fmt.Errorf("scan source aggregate: %w", err)
	}
	return nil
}

func (row sourceAggregateRow) course(
	identity sourceTermIdentity,
) domain.CourseTermAggregate {
	return domain.CourseTermAggregate{
		EducationLevel:      identity.EducationLevel,
		PeriodID:            identity.PeriodID,
		TermLabel:           identity.TermLabel,
		TermCode:            strings.TrimSpace(row.TermCode),
		CourseCode:          strings.TrimSpace(row.CourseCode),
		CourseName:          strings.TrimSpace(row.CourseName),
		ValidCount:          row.ValidCount,
		PassCount:           row.PassCount,
		FailCount:           row.FailCount,
		NumericScoreCount:   row.NumericScoreCount,
		NumericScoreSumX100: row.NumericScoreSumX100,
		Distribution:        row.Distribution,
	}
}

func (row sourceAggregateRow) instructor(
	identity sourceTermIdentity,
) (
	domain.InstructorCourseTermAggregate,
	bool,
) {
	name := normalizeTeacherName(row.TeacherName)
	if name == "" {
		return domain.InstructorCourseTermAggregate{}, false
	}
	sum := sha256.Sum256([]byte(name))
	return domain.InstructorCourseTermAggregate{
		EducationLevel:      identity.EducationLevel,
		PeriodID:            identity.PeriodID,
		TermLabel:           identity.TermLabel,
		TermCode:            strings.TrimSpace(row.TermCode),
		CourseCode:          strings.TrimSpace(row.CourseCode),
		CourseName:          strings.TrimSpace(row.CourseName),
		TeacherKey:          hex.EncodeToString(sum[:]),
		TeacherName:         name,
		ClassCount:          row.ClassCount,
		ValidCount:          row.ValidCount,
		PassCount:           row.PassCount,
		FailCount:           row.FailCount,
		NumericScoreCount:   row.NumericScoreCount,
		NumericScoreSumX100: row.NumericScoreSumX100,
		Distribution:        row.Distribution,
	}, true
}

var (
	undergraduateSourceTermPattern = regexp.MustCompile(
		`^([0-9]{4})(夏季|秋季|春季)学期$`,
	)
	graduateSourceTermPattern = regexp.MustCompile(
		`^([0-9]{4})-([0-9]{4})(夏秋|春季)$`,
	)
)

type sourceTermIdentity struct {
	EducationLevel string
	PeriodID       string
	TermLabel      string
}

// normalizeSourceTerm accepts only the known source term_id formats and
// verifies that their academic year and ordinal agree with term_code.
func normalizeSourceTerm(termID, termCode string) (sourceTermIdentity, bool) {
	termID = strings.TrimSpace(termID)
	termCode = strings.TrimSpace(termCode)
	if match := undergraduateSourceTermPattern.FindStringSubmatch(termID); len(match) == 3 {
		startYear, err := strconv.Atoi(match[1])
		if err != nil {
			return sourceTermIdentity{}, false
		}
		ordinal := map[string]string{
			"夏季": "1",
			"秋季": "2",
			"春季": "3",
		}[match[2]]
		if match[2] == "春季" {
			startYear--
		}
		periodID := fmt.Sprintf("%d-%d-%s", startYear, startYear+1, ordinal)
		if termCode != periodID {
			return sourceTermIdentity{}, false
		}
		labelYear := startYear
		if ordinal == "3" {
			labelYear++
		}
		return sourceTermIdentity{
			EducationLevel: domain.EducationLevelUndergraduate,
			PeriodID:       periodID,
			TermLabel:      fmt.Sprintf("%d %s学期", labelYear, match[2]),
		}, true
	}
	match := graduateSourceTermPattern.FindStringSubmatch(termID)
	if len(match) != 4 {
		return sourceTermIdentity{}, false
	}
	startYear, startErr := strconv.Atoi(match[1])
	endYear, endErr := strconv.Atoi(match[2])
	if startErr != nil || endErr != nil || endYear != startYear+1 {
		return sourceTermIdentity{}, false
	}
	ordinal := "11"
	legacyOrdinal := "2"
	if match[3] == "春季" {
		ordinal = "12"
		legacyOrdinal = "3"
	}
	periodID := fmt.Sprintf("%d:%s", startYear, ordinal)
	expectedTermCode := fmt.Sprintf(
		"%d-%d-%s",
		startYear,
		endYear,
		legacyOrdinal,
	)
	if termCode != expectedTermCode {
		return sourceTermIdentity{}, false
	}
	return sourceTermIdentity{
		EducationLevel: domain.EducationLevelGraduate,
		PeriodID:       periodID,
		TermLabel: fmt.Sprintf(
			"%d-%d 学年%s学期",
			startYear,
			endYear,
			match[3],
		),
	}, true
}

func normalizeTeacherName(value string) string {
	name := strings.Join(strings.Fields(strings.TrimSpace(value)), "")
	if strings.ContainsAny(name, ",，、;/；") {
		return ""
	}
	return name
}
