package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/daeuniverse/dae-wing/cmd/internal"
	"github.com/daeuniverse/dae-wing/common"
	"github.com/daeuniverse/dae-wing/dae"
	"github.com/daeuniverse/dae-wing/db"
	"github.com/daeuniverse/dae-wing/graphql"
	"github.com/daeuniverse/dae-wing/graphql/service/config"

	"github.com/daeuniverse/dae-wing/graphql/service/subscription"
	"github.com/daeuniverse/dae-wing/webrender"
	"github.com/golang-jwt/jwt/v5"
	"github.com/graph-gophers/graphql-go/relay"
	"github.com/rs/cors"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"gopkg.in/natefinch/lumberjack.v2"
)

func init() {
	runCmd.PersistentFlags().StringVarP(&cfgDir, "config", "c", filepath.Join("/etc", db.AppName), "config directory")
	runCmd.PersistentFlags().StringVarP(&listen, "listen", "l", "0.0.0.0:2023", "listening address")
	runCmd.PersistentFlags().BoolVar(&apiOnly, "api-only", false, "run graphql backend without dae")
	runCmd.PersistentFlags().StringVar(&logFile, "logfile", "", "Log file to write. Empty means writing to stdout and stderr.")
	runCmd.PersistentFlags().IntVar(&logFileMaxSize, "logfile-maxsize", 30, "Unit: MB. The maximum size in megabytes of the log file before it gets rotated.")
	runCmd.PersistentFlags().IntVar(&logFileMaxBackups, "logfile-maxbackups", 3, "The maximum number of old log files to retain.")
	runCmd.PersistentFlags().BoolVarP(&disableTimestamp, "disable-timestamp", "", false, "disable timestamp")
}

func _errorExit(err error) {
	// Notify to Close().
	logrus.Errorf("Exiting: %v", err)
	dae.ChReloadConfigs <- nil
	<-dae.GracefullyExit
}

func errorExit(err error) {
	_errorExit(err)
	os.Exit(1)
}

var (
	cfgDir            string
	logFile           string
	logFileMaxSize    int
	logFileMaxBackups int
	disableTimestamp  bool
	listen            string
	apiOnly           bool

	runCmd = &cobra.Command{
		Use:   "run",
		Short: "Run " + db.AppName + " in the foreground",
		Run: func(cmd *cobra.Command, args []string) {
			if cfgDir == "" {
				logrus.Fatalln("Argument \"--config\" or \"-c\" is required but not provided.")
			}
			if err := os.MkdirAll(cfgDir, 0750); err != nil && !os.IsExist(err) {
				logrus.Fatalln(err)
			}

			// Require "sudo" if necessary.
			if !apiOnly {
				internal.AutoSu()
			}

			// Read config from --config cfgDir.
			if err := db.InitDatabase(cfgDir); err != nil {
				logrus.Fatalln("Failed to init db:", err)
			}

			subscription.UpdateAll(context.TODO())

			// Run dae.
			var logOpts *lumberjack.Logger
			if logFile != "" {
				logOpts = &lumberjack.Logger{
					Filename:   logFile,
					MaxSize:    logFileMaxSize,
					MaxAge:     0,
					MaxBackups: logFileMaxBackups,
					LocalTime:  true,
					Compress:   true,
				}
				logrus.SetOutput(logOpts)
				db.SetOutput(logOpts)
			}
			go func() {
				if err := dae.Run(
					logrus.StandardLogger(),
					dae.EmptyConfig,
					[]string{cfgDir},
					disableTimestamp,
					apiOnly,
				); err != nil {
					logrus.Fatalln("dae.Run:", err)
				}
				os.Exit(1)
			}()
			// Reload with running state.
			if err := restoreRunningState(); err != nil {
				logrus.Warnln("Failed to restore last running state:", err)
			}

			// ListenAndServe GraphQL.
			schema, err := graphql.Schema()
			if err != nil {
				errorExit(err)
			}
			mux := http.NewServeMux()
			mux.Handle("/graphql", auth(cors.AllowAll().Handler(&relay.Handler{Schema: schema})))
			if err = webrender.Handle(mux); err != nil {
				errorExit(err)
			}
			go func() {
				host, port, _ := net.SplitHostPort(listen)
				if host == "0.0.0.0" || host == "::" {
					addrs, err := common.GetIfAddrs()
					if err == nil {
						for _, addr := range addrs {
							addr = net.JoinHostPort(addr, port)
							logrus.Printf("Listen on http://%v", addr)
						}
						goto listenAndServe
					}
				}
				logrus.Printf("Listen on %v", listen)
			listenAndServe:
				if err = http.ListenAndServe(listen, mux); err != nil {
					errorExit(err)
				}
			}()
			sigs := make(chan os.Signal, 1)
			signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGKILL, syscall.SIGILL)
			for sig := range sigs {
				_errorExit(errors.New(sig.String()))
				return
			}
		},
	}
)

func restoreRunningState() (err error) {
	reload, err := shouldReload()
	if err != nil {
		return err
	}
	if !reload {
		return nil
	}
	tx := db.BeginTx(context.TODO())
	// Reload.
	if _, err = config.Run(tx, false); err != nil {
		tx.Rollback()

		// Another tx.
		// Set running = false.
		tx2 := db.BeginTx(context.TODO())
		var sys db.System
		if err2 := tx2.Model(&sys).Select("id").First(&sys).Error; err2 != nil {
			tx2.Rollback()
			return fmt.Errorf("%w; %v", err, err2)
		}
		if err2 := tx2.Model(&sys).Updates(map[string]interface{}{
			"running": false,
		}).Error; err2 != nil {
			tx2.Rollback()
			return fmt.Errorf("%w; %v", err, err2)
		}
		tx2.Commit()
		return err
	}
	tx.Commit()
	return nil
}

func shouldReload() (ok bool, err error) {
	var sys db.System
	if err := db.DB(context.TODO()).Model(&db.System{}).FirstOrCreate(&sys).Error; err != nil {
		return false, err
	}
	if !sys.Running {
		return false, nil
	}
	var m db.Config
	q := db.DB(context.TODO()).Model(&db.Config{}).
		Where("selected = ?", true).
		First(&m)
	if q.Error != nil {
		return false, q.Error
	}
	if q.RowsAffected == 0 {
		// Data inconsistency.
		logrus.Warnln("Data inconsistency detected: no selected config but last state is running")
		_ = db.DB(context.TODO()).Model(&sys).Update("running", false).Error
		return false, nil
	}
	return true, nil
}

func auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		var user db.User
		token, err := jwt.Parse(authorization, func(token *jwt.Token) (interface{}, error) {
			// Don't forget to validate the alg is what you expect:
			if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
			}
			// Get corresponding secret.
			subject, err := token.Claims.GetSubject()
			if err != nil {
				return nil, err
			}
			q := db.DB(context.TODO()).Model(&db.User{}).Where("username = ?", subject).First(&user)
			if q.Error != nil {
				return nil, q.Error
			}
			if q.RowsAffected == 0 {
				return nil, fmt.Errorf("no such user")
			}
			return []byte(user.JwtSecret), nil
		})
		ctx := context.Background()
		if err == nil {
			if claims, ok := token.Claims.(jwt.MapClaims); ok && token.Valid {
				if expireAt, err := token.Claims.GetExpirationTime(); err == nil && time.Now().Before(expireAt.Time) {
					ctx = context.WithValue(ctx, "role", claims["role"])
					ctx = context.WithValue(ctx, "user", &user)
				}
			}
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
