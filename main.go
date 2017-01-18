package main

import (
	"database/sql"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"

	"github.com/Sirupsen/logrus"
	"github.com/bradleyfalzon/gopherci-web/internal/gopherci"
	"github.com/bradleyfalzon/gopherci-web/internal/users"
	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
	"github.com/joho/godotenv"
	"github.com/pkg/errors"
	"github.com/pressly/chi"
	"github.com/pressly/chi/middleware"
	migrate "github.com/rubenv/sql-migrate"
)

var (
	db        *sql.DB
	um        *users.UserManager
	gciClient *gopherci.Client
	templates *template.Template // templates contains all the html templates
	logger    = logrus.New()
)

func main() {
	logger.Println("Starting GopherCI-web")

	_ = godotenv.Load() // .env is not critical

	listen := os.Getenv("HTTP_LISTEN")

	logger.Printf("Connecting to %q db name: %q, username: %q, host: %q, port: %q",
		os.Getenv("DB_DRIVER"), os.Getenv("DB_DATABASE"), os.Getenv("DB_USERNAME"), os.Getenv("DB_HOST"), os.Getenv("DB_PORT"),
	)

	// TODO strict mode
	dsn := fmt.Sprintf(`%s:%s@tcp(%s:%s)/%s?charset=utf8&collation=utf8_unicode_ci&timeout=6s&time_zone='%%2B00:00'&parseTime=true`,
		os.Getenv("DB_USERNAME"), os.Getenv("DB_PASSWORD"), os.Getenv("DB_HOST"), os.Getenv("DB_PORT"), os.Getenv("DB_DATABASE"),
	)
	var err error
	db, err = sql.Open(os.Getenv("DB_DRIVER"), dsn)
	if err != nil {
		logger.WithError(err).Fatal("Error setting up DB")
	}
	dbx := sqlx.NewDb(db, os.Getenv("DB_DRIVER"))

	// Do DB migrations
	migrations := &migrate.FileMigrationSource{Dir: "migrations"}
	migrate.SetTable("migrations-web")
	direction := migrate.Up
	migrateMax := 0
	if len(os.Args) > 1 && os.Args[1] == "down" {
		direction = migrate.Down
		migrateMax = 1
	}
	n, err := migrate.ExecMax(db, os.Getenv("DB_DRIVER"), migrations, direction, migrateMax)
	logger.Printf("Applied %d migrations to database", n)
	if err != nil {
		logger.Fatal(errors.Wrap(err, "could not execute all migrations"))
	}

	// Initialise html templates
	if templates, err = template.ParseGlob("templates/*.tmpl"); err != nil {
		logger.WithError(err).Fatal("could not parse html templates")
	}

	// GopherCI client
	// TODO strict mode
	logger.Printf("Connecting to GopherCI DB %q db name: %q, username: %q, host: %q, port: %q",
		os.Getenv("GCI_DB_DRIVER"), os.Getenv("GCI_DB_DATABASE"), os.Getenv("GCI_DB_USERNAME"), os.Getenv("GCI_DB_HOST"), os.Getenv("GCI_DB_PORT"),
	)
	dsn = fmt.Sprintf(`%s:%s@tcp(%s:%s)/%s?charset=utf8&collation=utf8_unicode_ci&timeout=6s&time_zone='%%2B00:00'&parseTime=true`,
		os.Getenv("GCI_DB_USERNAME"), os.Getenv("GCI_DB_PASSWORD"), os.Getenv("GCI_DB_HOST"), os.Getenv("GCI_DB_PORT"), os.Getenv("GCI_DB_DATABASE"),
	)
	gciDB, err := sql.Open(os.Getenv("GCI_DB_DRIVER"), dsn)
	if err != nil {
		logger.WithError(err).Fatal("could not connect to GopherCI db")
	}
	gciDBx := sqlx.NewDb(gciDB, os.Getenv("GCI_DB_DRIVER"))
	gciClient = gopherci.New(gciDBx)

	r := chi.NewRouter()
	r.Use(middleware.RealIP) // Blindly accept XFF header, ensure LB overwrites it
	r.Use(middleware.DefaultCompress)
	r.Use(middleware.Recoverer)
	r.Use(middleware.NoCache)
	r.Use(middleware.DefaultLogger)
	r.Use(SessionMiddleware)
	workDir, _ := os.Getwd()
	r.FileServer("/static", http.Dir(filepath.Join(workDir, "static")))
	r.NotFound(notFoundHandler)

	r.Get("/", homeHandler)
	r.Get("/logout", logoutHandler)
	r.Post("/stripe/event", stripeEventHandler)
	r.Route("/console", func(r chi.Router) {
		r.Use(MustBeUserMiddleware)
		r.Get("/", consoleIndexHandler)
		r.Post("/install-state", consoleInstallStateHandler)
		r.Route("/billing", func(r chi.Router) {
			r.Get("/", consoleBillingHandler)
			r.Post("/process/:planID", consoleBillingProcessHandler)
			r.Post("/cancel", consoleBillingCancelHandler)
		})
	})

	// UserManager
	switch {
	case os.Getenv("GITHUB_OAUTH_CLIENT_ID") == "":
		logger.Fatal("GITHUB_OAUTH_CLIENT_ID is not set")
	case os.Getenv("GITHUB_OAUTH_CLIENT_SECRET") == "":
		logger.Fatal("GITHUB_OAUTH_CLIENT_SECRET is not set")
	}
	um = users.NewUserManager(logger.WithField("pkg", "users"), dbx, os.Getenv("GITHUB_OAUTH_CLIENT_ID"), os.Getenv("GITHUB_OAUTH_CLIENT_SECRET"), os.Getenv("STRIPE_SECRET_KEY"))
	r.Get("/gh/login", um.OAuthLoginHandler)
	r.Get("/gh/callback", um.OAuthCallbackHandler)

	logger.Println("Listening on", listen)
	logger.Fatal(http.ListenAndServe(listen, r))
}
