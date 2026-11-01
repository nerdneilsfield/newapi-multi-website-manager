package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"

	"github.com/nerdneilsfield/newapi-multi-website-manager/internal/manager"
	"github.com/nerdneilsfield/newapi-multi-website-manager/internal/store"
	"github.com/nerdneilsfield/newapi-multi-website-manager/internal/web"
)

func newServeCmd() *cobra.Command {
	var addr, dbPath string
	var interval time.Duration
	c := &cobra.Command{
		Use:   "serve",
		Short: "Run the multi-platform manager web UI and scheduler.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			log := loggerFrom(cmd)
			st, err := store.Open(dbPath)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			if err = st.InterruptRunning(); err != nil {
				return err
			}
			if err = bootstrapAdmin(st, cmd); err != nil {
				return err
			}

			mgr := manager.New(cmd.Context(), st, log, interval)
			if err = mgr.Reload(); err != nil {
				return err
			}
			srv := &http.Server{
				Addr: addr, Handler: web.New(st, mgr).Handler(), ReadHeaderTimeout: 10 * time.Second,
				ReadTimeout: 30 * time.Second, WriteTimeout: 2 * time.Minute, IdleTimeout: 2 * time.Minute,
			}
			go func() {
				<-cmd.Context().Done()
				shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = srv.Shutdown(shutdown)
			}()
			log.Info("listening", zap.String("addr", addr), zap.String("db", dbPath))
			err = srv.ListenAndServe()
			mgr.Stop()
			mgr.Wait()
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		},
	}
	c.Flags().StringVar(&addr, "addr", "127.0.0.1:8080", "listen address")
	c.Flags().StringVar(&dbPath, "db", "napm.db", "SQLite database path")
	c.Flags().DurationVar(&interval, "interval", 600*time.Millisecond, "minimum gap between requests to one platform (new-api allows 360 per 3 minutes per IP)")
	return c
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func randomPassword() string {
	b := make([]byte, 9)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// bootstrapAdmin creates the first root admin on an empty database, from
// NAPM_ADMIN_USER / NAPM_ADMIN_PASSWORD or a random password printed once.
func bootstrapAdmin(st *store.Store, cmd *cobra.Command) error {
	n, err := st.CountAdmins()
	if err != nil || n > 0 {
		return err
	}
	user, pass := envOr("NAPM_ADMIN_USER", "admin"), envOr("NAPM_ADMIN_PASSWORD", "")
	if pass != "" && len(pass) < 8 { // the same minimum the web page enforces
		return errors.New("NAPM_ADMIN_PASSWORD 至少 8 位")
	}
	if pass == "" {
		pass = randomPassword()
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "首次启动，已创建 root 管理员\n  用户名: %s\n  密码:   %s\n(只显示这一次，登录后请在「管理员」页修改)\n", user, pass)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pass), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = st.CreateAdmin(user, string(hash), true, nil)
	return err
}
