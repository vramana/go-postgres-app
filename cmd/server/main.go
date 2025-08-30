package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
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
	log.Printf("listening on %s", addr)
	log.Println("routes:")
    for _, r := range routes { log.Printf("  %s %s", r.method, r.path) }
	if err := http.ListenAndServe(addr, logRequest(mux)); err != nil {
		log.Fatal(err)
	}
}

func getenv(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func logRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
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
