package main

import (
    "bytes"
    "context"
    "encoding/json"
    "fmt"
    "log"
    "log/slog"
    "math/rand"
    "net/http"
    "os"
    "strconv"
    "time"
    "strings"

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
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200); w.Write([]byte("ok")) })
	mux.HandleFunc("/students", srv.handleStudents)
	mux.HandleFunc("/students/", srv.handleStudentByID)
	mux.HandleFunc("/attendances", srv.handleAttendances)

    addr := getenv("HTTP_ADDR", ":8080")
    logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
    logger.Info("listening", slog.String("addr", addr))
    logger.Info("routes")
    for _, r := range routes { logger.Info("route", slog.String("method", r.method), slog.String("path", r.path)) }

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

type statusRecorder struct { http.ResponseWriter; status int }
func (sr *statusRecorder) WriteHeader(code int) { sr.status = code; sr.ResponseWriter.WriteHeader(code) }

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
		`CREATE TABLE IF NOT EXISTS attendances (
            id BIGSERIAL PRIMARY KEY,
            student_id BIGINT NOT NULL REFERENCES students(id) ON DELETE CASCADE,
            status TEXT NOT NULL CHECK (status IN ('present','absent','late')),
            marked_at TIMESTAMPTZ NOT NULL DEFAULT now()
        );`,
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

func (s *Server) handleAttendances(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var in struct {
			StudentID int64  `json:"student_id"`
			Status    string `json:"status"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if in.StudentID == 0 || (in.Status != "present" && in.Status != "absent" && in.Status != "late") {
			http.Error(w, "invalid input", 400)
			return
		}
		row := s.db.QueryRow(r.Context(), "INSERT INTO attendances(student_id, status) VALUES($1,$2) RETURNING id, student_id, status, marked_at", in.StudentID, in.Status)
		var a Attendance
		if err := row.Scan(&a.ID, &a.StudentID, &a.Status, &a.MarkedAt); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, 201, a)
	case http.MethodGet:
		// optional filters: student_id, date (YYYY-MM-DD)
		q := "SELECT id, student_id, status, marked_at FROM attendances"
		args := []any{}
		where := ""
		if sid := r.URL.Query().Get("student_id"); sid != "" {
			if _, err := strconv.ParseInt(sid, 10, 64); err == nil {
				where = appendWhere(where, "student_id = $1")
				args = append(args, sid)
			}
		}
		if d := r.URL.Query().Get("date"); d != "" {
			where = appendWhere(where, "DATE(marked_at) = $2")
			args = append(args, d)
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
			if err := rows.Scan(&a.ID, &a.StudentID, &a.Status, &a.MarkedAt); err != nil {
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
	rand.Seed(time.Now().UnixNano())
	statuses := []string{"present", "absent", "late"}
	for {
		select {
		case <-time.After(interval):
			// pick a random student id from DB
			var id int64
			err := s.db.QueryRow(context.Background(), "SELECT id FROM students ORDER BY random() LIMIT 1").Scan(&id)
			if err != nil {
				// nothing to do if no students yet
				continue
			}
			status := statuses[rand.Intn(len(statuses))]
			// call internal HTTP endpoint
			body := fmt.Sprintf(`{"student_id":%d,"status":"%s"}`, id, status)
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
