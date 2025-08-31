package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

type Server struct {
	db *pgxpool.Pool
}

type Student struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type Attendance struct {
	ID        int64     `json:"id"`
	StudentID int64     `json:"student_id"`
	Status    string    `json:"status"` // present, absent, late
	MarkedAt  time.Time `json:"marked_at"`
	SessionID *int64    `json:"session_id,omitempty"`
	Reason    *string   `json:"reason,omitempty"`
	Note      *string   `json:"note,omitempty"`
}

type Class struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type Session struct {
	ID      int64     `json:"id"`
	ClassID int64     `json:"class_id"`
	Date    time.Time `json:"date"`
	Period  int       `json:"period"`
}

func main() {
	_ = godotenv.Load()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		user := getenv("POSTGRES_USER", "postgres")
		pass := getenv("POSTGRES_PASSWORD", "postgres")
		host := getenv("POSTGRES_HOST", "localhost")
		port := getenv("POSTGRES_PORT", "5432")
		db := getenv("POSTGRES_DB", "appdb")
		dsn = fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable", user, pass, host, port, db)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatalf("db pool create: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		log.Fatalf("db ping: %v", err)
	}

	if err := migrate(ctx, pool); err != nil {
		log.Fatalf("migrate: %v", err)
	}

	srv := &Server{db: pool}

	// Bootstrap students if empty
	if err := bootstrapStudents(ctx, pool, 100); err != nil {
		log.Fatalf("bootstrap students: %v", err)
	}

	mux := http.NewServeMux()

	routes := []struct{ method, path string }{
		{http.MethodGet, "/healthz"},
		{http.MethodPost, "/students"},
		{http.MethodGet, "/students"},
		{http.MethodGet, "/students/{id}"},
		{http.MethodDelete, "/students/{id}"},
		{http.MethodPost, "/attendances"},
		{http.MethodGet, "/attendances"},
		{http.MethodPost, "/classes"},
		{http.MethodGet, "/classes"},
		{http.MethodGet, "/classes/{id}"},
		{http.MethodPost, "/classes/{id}/enroll"},
		{http.MethodGet, "/classes/{id}/students"},
		{http.MethodPost, "/sessions"},
		{http.MethodGet, "/sessions"},
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200); w.Write([]byte("ok")) })
	mux.HandleFunc("/students", srv.handleStudents)
	mux.HandleFunc("/students/", srv.handleStudentByID)
	mux.HandleFunc("/attendances", srv.handleAttendances)
	mux.HandleFunc("/classes", srv.handleClasses)
	mux.HandleFunc("/classes/", srv.handleClassByID)
	mux.HandleFunc("/sessions", srv.handleSessions)

	addr := getenv("HTTP_ADDR", ":8080")
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	logger.Info("listening", slog.String("addr", addr))
	logger.Info("routes")
	for _, r := range routes {
		logger.Info("route", slog.String("method", r.method), slog.String("path", r.path))
	}

	// Start background job to randomly mark attendance
	go startRandomAttendanceJob(context.Background(), srv)
	if err := http.ListenAndServe(addr, slogRequest(logger, mux)); err != nil {
		log.Fatal(err)
	}
}

func getenv(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

func slogRequest(l *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sr := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(sr, r)
		l.Info("request", slog.String("method", r.Method), slog.String("path", r.URL.Path), slog.Int("status", sr.status), slog.Int64("dur_ms", time.Since(start).Milliseconds()))
	})
}

func migrate(ctx context.Context, db *pgxpool.Pool) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS students (
            id BIGSERIAL PRIMARY KEY,
            name TEXT NOT NULL
        );`,
		`CREATE TABLE IF NOT EXISTS classes (
            id BIGSERIAL PRIMARY KEY,
            name TEXT NOT NULL UNIQUE
        );`,
		`CREATE TABLE IF NOT EXISTS enrollments (
            student_id BIGINT NOT NULL REFERENCES students(id) ON DELETE CASCADE,
            class_id BIGINT NOT NULL REFERENCES classes(id) ON DELETE CASCADE,
            PRIMARY KEY(student_id, class_id)
        );`,
		`CREATE TABLE IF NOT EXISTS sessions (
            id BIGSERIAL PRIMARY KEY,
            class_id BIGINT NOT NULL REFERENCES classes(id) ON DELETE CASCADE,
            date DATE NOT NULL,
            period INT NOT NULL,
            UNIQUE(class_id, date, period)
        );`,
		`CREATE TABLE IF NOT EXISTS attendances (
            id BIGSERIAL PRIMARY KEY,
            student_id BIGINT NOT NULL REFERENCES students(id) ON DELETE CASCADE,
            status TEXT NOT NULL CHECK (status IN ('present','absent','late')),
            marked_at TIMESTAMPTZ NOT NULL DEFAULT now(),
            session_id BIGINT NULL REFERENCES sessions(id) ON DELETE SET NULL,
            reason TEXT NULL,
            note TEXT NULL
        );`,
		// Backfill columns in case attendances existed from an older version
		`ALTER TABLE attendances ADD COLUMN IF NOT EXISTS session_id BIGINT NULL REFERENCES sessions(id) ON DELETE SET NULL;`,
		`ALTER TABLE attendances ADD COLUMN IF NOT EXISTS reason TEXT NULL;`,
		`ALTER TABLE attendances ADD COLUMN IF NOT EXISTS note TEXT NULL;`,
		`CREATE INDEX IF NOT EXISTS idx_att_student ON attendances(student_id);`,
		`CREATE INDEX IF NOT EXISTS idx_att_session ON attendances(session_id);`,
		`CREATE INDEX IF NOT EXISTS idx_att_marked_at ON attendances(marked_at);`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// Handlers
func (s *Server) handleStudents(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var in struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if in.Name == "" {
			http.Error(w, "name required", 400)
			return
		}
		row := s.db.QueryRow(r.Context(), "INSERT INTO students(name) VALUES($1) RETURNING id, name", in.Name)
		var st Student
		if err := row.Scan(&st.ID, &st.Name); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, 201, st)
	case http.MethodGet:
		rows, err := s.db.Query(r.Context(), "SELECT id, name FROM students ORDER BY id")
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer rows.Close()
		list := []Student{}
		for rows.Next() {
			var st Student
			if err := rows.Scan(&st.ID, &st.Name); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			list = append(list, st)
		}
		writeJSON(w, 200, list)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleStudentByID(w http.ResponseWriter, r *http.Request) {
	idStr := r.URL.Path[len("/students/"):]
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid id", 400)
		return
	}
	switch r.Method {
	case http.MethodGet:
		var st Student
		row := s.db.QueryRow(r.Context(), "SELECT id, name FROM students WHERE id=$1", id)
		if err := row.Scan(&st.ID, &st.Name); err != nil {
			http.Error(w, "not found", 404)
			return
		}
		writeJSON(w, 200, st)
	case http.MethodDelete:
		_, err := s.db.Exec(r.Context(), "DELETE FROM students WHERE id=$1", id)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(204)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// Classes and enrollments
func (s *Server) handleClasses(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var in struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if in.Name == "" {
			http.Error(w, "name required", 400)
			return
		}
		row := s.db.QueryRow(r.Context(), "INSERT INTO classes(name) VALUES($1) RETURNING id, name", in.Name)
		var c Class
		if err := row.Scan(&c.ID, &c.Name); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, 201, c)
	case http.MethodGet:
		rows, err := s.db.Query(r.Context(), "SELECT id, name FROM classes ORDER BY id")
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer rows.Close()
		out := []Class{}
		for rows.Next() {
			var c Class
			if err := rows.Scan(&c.ID, &c.Name); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			out = append(out, c)
		}
		writeJSON(w, 200, out)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleClassByID(w http.ResponseWriter, r *http.Request) {
	// supports: GET /classes/{id}, POST /classes/{id}/enroll, GET /classes/{id}/students
	path := r.URL.Path
	base := "/classes/"
	if !strings.HasPrefix(path, base) {
		w.WriteHeader(404)
		return
	}
	rest := strings.TrimPrefix(path, base)
	parts := strings.Split(rest, "/")
	if len(parts) == 0 {
		w.WriteHeader(404)
		return
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.Error(w, "invalid id", 400)
		return
	}
	// sub-routes
	if len(parts) == 1 {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var c Class
		row := s.db.QueryRow(r.Context(), "SELECT id, name FROM classes WHERE id=$1", id)
		if err := row.Scan(&c.ID, &c.Name); err != nil {
			http.Error(w, "not found", 404)
			return
		}
		writeJSON(w, 200, c)
		return
	}
	switch parts[1] {
	case "enroll":
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var in struct {
			StudentID int64 `json:"student_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if in.StudentID == 0 {
			http.Error(w, "student_id required", 400)
			return
		}
		_, err := s.db.Exec(r.Context(), "INSERT INTO enrollments(student_id, class_id) VALUES($1,$2) ON CONFLICT DO NOTHING", in.StudentID, id)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(204)
	case "students":
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		rows, err := s.db.Query(r.Context(), "SELECT s.id, s.name FROM students s JOIN enrollments e ON e.student_id=s.id WHERE e.class_id=$1 ORDER BY s.id", id)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer rows.Close()
		out := []Student{}
		for rows.Next() {
			var st Student
			if err := rows.Scan(&st.ID, &st.Name); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			out = append(out, st)
		}
		writeJSON(w, 200, out)
	default:
		w.WriteHeader(404)
	}
}

// Sessions
func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var in struct {
			ClassID int64  `json:"class_id"`
			Date    string `json:"date"`
			Period  int    `json:"period"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if in.ClassID == 0 || in.Period <= 0 || in.Date == "" {
			http.Error(w, "invalid input", 400)
			return
		}
		row := s.db.QueryRow(r.Context(), "INSERT INTO sessions(class_id, date, period) VALUES($1,$2,$3) ON CONFLICT(class_id,date,period) DO UPDATE SET period=EXCLUDED.period RETURNING id, class_id, date, period", in.ClassID, in.Date, in.Period)
		var sss Session
		if err := row.Scan(&sss.ID, &sss.ClassID, &sss.Date, &sss.Period); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, 201, sss)
	case http.MethodGet:
		q := "SELECT id, class_id, date, period FROM sessions"
		args := []any{}
		where := ""
		if cid := r.URL.Query().Get("class_id"); cid != "" {
			where = appendWhere(where, fmt.Sprintf("class_id = $%d", len(args)+1))
			args = append(args, cid)
		}
		if d := r.URL.Query().Get("date"); d != "" {
			where = appendWhere(where, fmt.Sprintf("date = $%d", len(args)+1))
			args = append(args, d)
		}
		if where != "" {
			q += " WHERE " + where
		}
		q += " ORDER BY date DESC, class_id, period"
		rows, err := s.db.Query(r.Context(), q, args...)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer rows.Close()
		out := []Session{}
		for rows.Next() {
			var se Session
			if err := rows.Scan(&se.ID, &se.ClassID, &se.Date, &se.Period); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			out = append(out, se)
		}
		writeJSON(w, 200, out)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}
func (s *Server) handleAttendances(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var in struct {
			StudentID int64   `json:"student_id"`
			Status    string  `json:"status"`
			SessionID *int64  `json:"session_id"`
			Reason    *string `json:"reason"`
			Note      *string `json:"note"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if in.StudentID == 0 || (in.Status != "present" && in.Status != "absent" && in.Status != "late") {
			http.Error(w, "invalid input", 400)
			return
		}
		row := s.db.QueryRow(r.Context(), "INSERT INTO attendances(student_id, status, session_id, reason, note) VALUES($1,$2,$3,$4,$5) RETURNING id, student_id, status, marked_at, session_id, reason, note", in.StudentID, in.Status, in.SessionID, in.Reason, in.Note)
		var a Attendance
		if err := row.Scan(&a.ID, &a.StudentID, &a.Status, &a.MarkedAt, &a.SessionID, &a.Reason, &a.Note); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, 201, a)
	case http.MethodGet:
		q := "SELECT id, student_id, status, marked_at, session_id, reason, note FROM attendances"
		args := []any{}
		where := ""
		if sid := r.URL.Query().Get("student_id"); sid != "" {
			if _, err := strconv.ParseInt(sid, 10, 64); err == nil {
				where = appendWhere(where, fmt.Sprintf("student_id = $%d", len(args)+1))
				args = append(args, sid)
			}
		}
		if d := r.URL.Query().Get("date"); d != "" {
			where = appendWhere(where, fmt.Sprintf("DATE(marked_at) = $%d", len(args)+1))
			args = append(args, d)
		}
		if sess := r.URL.Query().Get("session_id"); sess != "" {
			if _, err := strconv.ParseInt(sess, 10, 64); err == nil {
				where = appendWhere(where, fmt.Sprintf("session_id = $%d", len(args)+1))
				args = append(args, sess)
			}
		}
		if cid := r.URL.Query().Get("class_id"); cid != "" {
			where = appendWhere(where, fmt.Sprintf("session_id IN (SELECT id FROM sessions WHERE class_id = $%d)", len(args)+1))
			args = append(args, cid)
		}
		if where != "" {
			q += " WHERE " + where
		}
		q += " ORDER BY marked_at DESC, id DESC"
		rows, err := s.db.Query(r.Context(), q, args...)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer rows.Close()
		list := []Attendance{}
		for rows.Next() {
			var a Attendance
			if err := rows.Scan(&a.ID, &a.StudentID, &a.Status, &a.MarkedAt, &a.SessionID, &a.Reason, &a.Note); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			list = append(list, a)
		}
		writeJSON(w, 200, list)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func appendWhere(where, cond string) string {
	if where == "" {
		return cond
	}
	return where + " AND " + cond
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func bootstrapStudents(ctx context.Context, db *pgxpool.Pool, n int) error {
	var cnt int
	if err := db.QueryRow(ctx, "SELECT COUNT(*) FROM students").Scan(&cnt); err != nil {
		return err
	}
	if cnt > 0 {
		return nil
	}
	batch := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		batch = append(batch, fmt.Sprintf("('%s')", fmt.Sprintf("Student %03d", i)))
	}
	_, err := db.Exec(ctx, "INSERT INTO students(name) VALUES "+strings.Join(batch, ","))
	return err
}

// Background job: periodically picks a random student and records attendance via HTTP
func startRandomAttendanceJob(ctx context.Context, s *Server) {
	intervalStr := getenv("RANDOM_ATT_INTERVAL", "500ms")
	interval, err := time.ParseDuration(intervalStr)
	if err != nil {
		interval = 30 * time.Second
	}
	client := &http.Client{Timeout: 5 * time.Second}
	statuses := []string{"present", "absent", "late"}
	for {
		select {
		case <-time.After(interval):
			// ensure at least 3 classes
			_, _ = s.db.Exec(ctx, "INSERT INTO classes(name) VALUES('Class A') ON CONFLICT DO NOTHING")
			_, _ = s.db.Exec(ctx, "INSERT INTO classes(name) VALUES('Class B') ON CONFLICT DO NOTHING")
			_, _ = s.db.Exec(ctx, "INSERT INTO classes(name) VALUES('Class C') ON CONFLICT DO NOTHING")
			// enroll students evenly
			rows, _ := s.db.Query(ctx, "SELECT id FROM students")
			var ids []int64
			for rows != nil && rows.Next() {
				var x int64
				_ = rows.Scan(&x)
				ids = append(ids, x)
			}
			if rows != nil {
				rows.Close()
			}
			// fetch class ids
			crows, _ := s.db.Query(ctx, "SELECT id FROM classes ORDER BY id LIMIT 3")
			classIDs := []int64{}
			for crows != nil && crows.Next() {
				var x int64
				_ = crows.Scan(&x)
				classIDs = append(classIDs, x)
			}
			if crows != nil {
				crows.Close()
			}
			for i, sid := range ids {
				if len(classIDs) == 0 {
					break
				}
				cid := classIDs[i%len(classIDs)]
				_, _ = s.db.Exec(ctx, "INSERT INTO enrollments(student_id, class_id) VALUES($1,$2) ON CONFLICT DO NOTHING", sid, cid)
			}
			// create today's sessions for each class, periods 1-3
			today := time.Now().Format("2006-01-02")
			for _, cid := range classIDs {
				for p := 1; p <= 3; p++ {
					_, _ = s.db.Exec(ctx, "INSERT INTO sessions(class_id, date, period) VALUES($1,$2,$3) ON CONFLICT DO NOTHING", cid, today, p)
				}
			}
			// pick random session and a student enrolled in that class
			var sessionID, classID int64
			err := s.db.QueryRow(ctx, "SELECT id, class_id FROM sessions ORDER BY random() LIMIT 1").Scan(&sessionID, &classID)
			if err != nil {
				// nothing to do if no students yet
				continue
			}
			var studentID int64
			err = s.db.QueryRow(ctx, "SELECT student_id FROM enrollments WHERE class_id=$1 ORDER BY random() LIMIT 1", classID).Scan(&studentID)
			if err != nil {
				continue
			}
			status := statuses[int(time.Now().UnixNano())%len(statuses)]
			// call internal HTTP endpoint
			body := fmt.Sprintf(`{"student_id":%d,"status":"%s","session_id":%d}`, studentID, status, sessionID)
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, getenv("INTERNAL_BASE_URL", "http://127.0.0.1:8080")+"/attendances",
				bytes.NewBufferString(body))
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err != nil {
				log.Printf("random attendance: request error: %v", err)
				continue
			}
			_ = resp.Body.Close()
			if resp.StatusCode >= 300 {
				log.Printf("random attendance: unexpected status %d", resp.StatusCode)
			}
		case <-ctx.Done():
			return
		}
	}
}
