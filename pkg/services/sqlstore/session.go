package sqlstore

import (
	"context"
	"errors"
	"reflect"
	"time"

	"xorm.io/xorm"

	"github.com/grafana/grafana/pkg/infra/log"
	"github.com/grafana/grafana/pkg/util/errutil"
	"github.com/grafana/grafana/pkg/util/retryer"
	"github.com/mattn/go-sqlite3"
)

var sessionLogger = log.New("sqlstore.session")
var ErrMaximumRetriesReached = errutil.NewBase(errutil.StatusInternal, "sqlstore.max-retries-reached")

type DBSession struct {
	*xorm.Session
	transactionOpen bool
	events          []interface{}
}

type DBTransactionFunc func(sess *DBSession) error

func (sess *DBSession) publishAfterCommit(msg interface{}) {
	sess.events = append(sess.events, msg)
}

func (sess *DBSession) PublishAfterCommit(msg interface{}) {
	sess.events = append(sess.events, msg)
}

func startSessionOrUseExisting(ctx context.Context, engine *xorm.Engine, beginTran bool) (*DBSession, bool, error) {
	value := ctx.Value(ContextSessionKey{})
	var sess *DBSession
	sess, ok := value.(*DBSession)

	if ok {
		ctxLogger := sessionLogger.FromContext(ctx)
		ctxLogger.Debug("reusing existing session", "transaction", sess.transactionOpen)
		sess.Session = sess.Session.Context(ctx)
		return sess, false, nil
	}

	newSess := &DBSession{Session: engine.NewSession(), transactionOpen: beginTran}
	if beginTran {
		err := newSess.Begin()
		if err != nil {
			return nil, false, err
		}
	}

	newSess.Session = newSess.Session.Context(ctx)

	return newSess, true, nil
}

// WithDbSession calls the callback with the session in the context (if exists).
// Otherwise it creates a new one that is closed upon completion.
// A session is stored in the context if sqlstore.InTransaction() has been been previously called with the same context (and it's not committed/rolledback yet).
// In case of sqlite3.ErrLocked or sqlite3.ErrBusy failure it will be retried at most five times before giving up.
func (ss *SQLStore) WithDbSession(ctx context.Context, callback DBTransactionFunc) error {
	return ss.withDbSession(ctx, ss.engine, callback)
}

// WithNewDbSession calls the callback with a new session that is closed upon completion.
// In case of sqlite3.ErrLocked or sqlite3.ErrBusy failure it will be retried at most five times before giving up.
func (ss *SQLStore) WithNewDbSession(ctx context.Context, callback DBTransactionFunc) error {
	sess := &DBSession{Session: ss.engine.NewSession(), transactionOpen: false}
	defer sess.Close()
	retry := 0
	return retryer.Retry(ss.retryOnLocks(ctx, callback, sess, retry), ss.dbCfg.QueryRetries, time.Millisecond*time.Duration(10), time.Second)
}

func (ss *SQLStore) retryOnLocks(ctx context.Context, callback DBTransactionFunc, sess *DBSession, retry int) func() (retryer.RetrySignal, error) {
	return func() (retryer.RetrySignal, error) {
		retry++

		err := callback(sess)

		ctxLogger := tsclogger.FromContext(ctx)

		var sqlError sqlite3.Error
		if errors.As(err, &sqlError) && (sqlError.Code == sqlite3.ErrLocked || sqlError.Code == sqlite3.ErrBusy) {
			ctxLogger.Info("Database locked, sleeping then retrying", "error", err, "retry", retry, "code", sqlError.Code)
			// retryer immediately returns the error (if there is one) without checking the response
			// therefore we only have to send it if we have reached the maximum retries
			if retry == ss.dbCfg.QueryRetries {
				return retryer.FuncError, ErrMaximumRetriesReached.Errorf("retry %d: %w", retry, err)
			}
			return retryer.FuncFailure, nil
		}

		if err != nil {
			return retryer.FuncError, err
		}

		return retryer.FuncComplete, nil
	}
}

func (ss *SQLStore) withDbSession(ctx context.Context, engine *xorm.Engine, callback DBTransactionFunc) error {
	sess, isNew, err := startSessionOrUseExisting(ctx, engine, false)
	if err != nil {
		return err
	}
	if isNew {
		defer sess.Close()
	}
	retry := 0
	return retryer.Retry(ss.retryOnLocks(ctx, callback, sess, retry), ss.dbCfg.QueryRetries, time.Millisecond*time.Duration(10), time.Second)
}

func (sess *DBSession) InsertId(bean interface{}) (int64, error) {
	table := sess.DB().Mapper.Obj2Table(getTypeName(bean))

	if err := dialect.PreInsertId(table, sess.Session); err != nil {
		return 0, err
	}
	id, err := sess.Session.InsertOne(bean)
	if err != nil {
		return 0, err
	}
	if err := dialect.PostInsertId(table, sess.Session); err != nil {
		return 0, err
	}

	return id, nil
}

func getTypeName(bean interface{}) (res string) {
	t := reflect.TypeOf(bean)
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t.Name()
}
