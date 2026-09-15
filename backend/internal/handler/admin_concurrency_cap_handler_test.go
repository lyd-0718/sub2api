package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type capHandlerTestAdminService struct {
	service.AdminService
	account *service.Account
}

func (s *capHandlerTestAdminService) GetAccount(_ context.Context, id int64) (*service.Account, error) {
	if s.account == nil || s.account.ID != id {
		return nil, fmt.Errorf("account %d not found", id)
	}
	return s.account, nil
}

type capHandlerTestSetCapCall struct {
	AccountID int64
	Cap       int
	Reason    string
}

type capHandlerTestStore struct {
	records     map[int64]*service.AccountConcurrencyCap
	setCapCalls []capHandlerTestSetCapCall
	clearedFlap []int64
	pinned      map[int64]bool
}

func newCapHandlerTestStore() *capHandlerTestStore {
	return &capHandlerTestStore{
		records: make(map[int64]*service.AccountConcurrencyCap),
		pinned:  make(map[int64]bool),
	}
}

func (s *capHandlerTestStore) EffectiveCap(_ context.Context, accountID int64) (int, bool, error) {
	record, ok := s.records[accountID]
	if !ok {
		return 0, false, nil
	}
	return record.Cap, true, nil
}

func (s *capHandlerTestStore) GetCap(_ context.Context, accountID int64) (*service.AccountConcurrencyCap, error) {
	record, ok := s.records[accountID]
	if !ok {
		return nil, nil
	}
	clone := *record
	return &clone, nil
}

func (s *capHandlerTestStore) SetCap(_ context.Context, accountID int64, capValue int, reason string) error {
	s.setCapCalls = append(s.setCapCalls, capHandlerTestSetCapCall{AccountID: accountID, Cap: capValue, Reason: reason})
	record, ok := s.records[accountID]
	if !ok {
		record = &service.AccountConcurrencyCap{AccountID: accountID}
		s.records[accountID] = record
	}
	record.Cap = capValue
	record.Reason = reason
	return nil
}

func (s *capHandlerTestStore) RecordFlap(_ context.Context, _ int64, _ time.Time) (int, error) {
	return 0, nil
}

func (s *capHandlerTestStore) SetPinned(_ context.Context, accountID int64, pinned bool) error {
	if _, ok := s.records[accountID]; !ok {
		return service.ErrConcurrencyCapNotFound
	}
	s.pinned[accountID] = pinned
	s.records[accountID].Pinned = pinned
	return nil
}

func (s *capHandlerTestStore) ListRestricted(_ context.Context) ([]*service.AccountConcurrencyCap, error) {
	return nil, nil
}

// EffectiveCaps / ClearFlapEvents 属于 store 的可选扩展（批量软路径、解熔断）。
func (s *capHandlerTestStore) EffectiveCaps(_ context.Context, _ []int64) map[int64]int {
	return nil
}

func (s *capHandlerTestStore) ClearFlapEvents(_ context.Context, accountID int64) error {
	s.clearedFlap = append(s.clearedFlap, accountID)
	if record, ok := s.records[accountID]; ok {
		record.FlapEvents = nil
	}
	return nil
}

func newCapHandlerTestContext(method, target, body, id string) (*gin.Context, *httptest.ResponseRecorder) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, target, bytes.NewBufferString(body))
	request.Header.Set("Content-Type", "application/json")
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = request
	if id != "" {
		ctx.Params = gin.Params{{Key: "id", Value: id}}
	}
	return ctx, recorder
}

func newCapHandlerUnderTest(store service.ConcurrencyCapStore, account *service.Account) *AdminConcurrencyCapHandler {
	return NewAdminConcurrencyCapHandler(store, &capHandlerTestAdminService{account: account}, &config.Config{})
}

func decodeCapHandlerResponse(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var payload map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &payload))
	data, ok := payload["data"].(map[string]any)
	require.True(t, ok, "响应必须带 data 段：%s", recorder.Body.String())
	return data
}

func TestAdminConcurrencyCapHandlerGetReportsConfiguredAndEffective(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newCapHandlerTestStore()
	account := &service.Account{ID: 3, Platform: "kimi", Concurrency: 10}
	store.records[3] = &service.AccountConcurrencyCap{AccountID: 3, Cap: 1, Reason: "concurrency_403", RestrictedAt: time.Now()}
	handler := newCapHandlerUnderTest(store, account)

	ctx, recorder := newCapHandlerTestContext(http.MethodGet, "/admin/accounts/3/concurrency-cap", "", "3")
	handler.Get(ctx)

	require.Equal(t, http.StatusOK, recorder.Code)
	data := decodeCapHandlerResponse(t, recorder)
	require.Equal(t, true, data["known"])
	require.Equal(t, true, data["restricted"])
	require.Equal(t, float64(10), data["configured_concurrency"])
	require.Equal(t, float64(1), data["effective_concurrency"], "有效并发必须夹帽到 cap")
	require.Equal(t, float64(3), data["cap_max"])
}

func TestAdminConcurrencyCapHandlerGetWithoutRecordKeepsConfiguredConcurrency(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newCapHandlerTestStore()
	account := &service.Account{ID: 4, Platform: "openai", Concurrency: 10}
	handler := newCapHandlerUnderTest(store, account)

	ctx, recorder := newCapHandlerTestContext(http.MethodGet, "/admin/accounts/4/concurrency-cap", "", "4")
	handler.Get(ctx)

	require.Equal(t, http.StatusOK, recorder.Code)
	data := decodeCapHandlerResponse(t, recorder)
	require.Equal(t, false, data["known"], "无记录 = 不夹帽")
	require.Equal(t, false, data["restricted"])
	require.Equal(t, float64(10), data["effective_concurrency"])
	require.Nil(t, data["cap"])
}

func TestAdminConcurrencyCapHandlerUpdateSetsCapAndPinned(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newCapHandlerTestStore()
	account := &service.Account{ID: 5, Platform: "kimi", Concurrency: 3}
	store.records[5] = &service.AccountConcurrencyCap{AccountID: 5, Cap: 1, RestrictedAt: time.Now()}
	handler := newCapHandlerUnderTest(store, account)

	ctx, recorder := newCapHandlerTestContext(http.MethodPut, "/admin/accounts/5/concurrency-cap", `{"cap":2,"pinned":true}`, "5")
	handler.Update(ctx)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Len(t, store.setCapCalls, 1)
	require.Equal(t, 2, store.setCapCalls[0].Cap)
	require.Equal(t, int64(5), store.setCapCalls[0].AccountID)
	require.True(t, store.pinned[5])
	data := decodeCapHandlerResponse(t, recorder)
	require.Equal(t, true, data["pinned"])
	require.Equal(t, float64(2), data["effective_concurrency"])
}

func TestAdminConcurrencyCapHandlerUpdatePinnedWithoutRecordUsesConfiguredConcurrency(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newCapHandlerTestStore()
	account := &service.Account{ID: 6, Platform: "kimi", Concurrency: 3}
	handler := newCapHandlerUnderTest(store, account)

	ctx, recorder := newCapHandlerTestContext(http.MethodPut, "/admin/accounts/6/concurrency-cap", `{"pinned":true}`, "6")
	handler.Update(ctx)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Len(t, store.setCapCalls, 1)
	require.Equal(t, 3, store.setCapCalls[0].Cap, "补建记录必须用配置并发，避免给健康账号悄悄加帽")
	require.True(t, store.pinned[6])
}

func TestAdminConcurrencyCapHandlerUpdateClearFuseClearsFlaps(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newCapHandlerTestStore()
	account := &service.Account{ID: 7, Platform: "kimi", Concurrency: 3}
	now := time.Now()
	store.records[7] = &service.AccountConcurrencyCap{
		AccountID: 7,
		Cap:       1,
		FlapEvents: []time.Time{
			now.Add(-time.Hour), now.Add(-2 * time.Hour), now.Add(-3 * time.Hour),
		},
	}
	handler := newCapHandlerUnderTest(store, account)

	ctx, recorder := newCapHandlerTestContext(http.MethodPut, "/admin/accounts/7/concurrency-cap", `{"clear_fuse":true}`, "7")
	handler.Update(ctx)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, []int64{7}, store.clearedFlap)
	data := decodeCapHandlerResponse(t, recorder)
	require.Equal(t, float64(0), data["flap_count_7d"])
	require.Equal(t, false, data["fused"])
}

func TestAdminConcurrencyCapHandlerRejectsInvalidRequests(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newCapHandlerTestStore()
	account := &service.Account{ID: 8, Platform: "kimi", Concurrency: 3}
	handler := newCapHandlerUnderTest(store, account)

	t.Run("invalid account id", func(t *testing.T) {
		ctx, recorder := newCapHandlerTestContext(http.MethodGet, "/admin/accounts/abc/concurrency-cap", "", "abc")
		handler.Get(ctx)
		require.Equal(t, http.StatusBadRequest, recorder.Code)
	})

	t.Run("non positive cap", func(t *testing.T) {
		ctx, recorder := newCapHandlerTestContext(http.MethodPut, "/admin/accounts/8/concurrency-cap", `{"cap":0}`, "8")
		handler.Update(ctx)
		require.Equal(t, http.StatusBadRequest, recorder.Code)
		require.Empty(t, store.setCapCalls)
	})

	t.Run("pinned without record and without configured concurrency", func(t *testing.T) {
		unlimitedStore := newCapHandlerTestStore()
		unlimited := newCapHandlerUnderTest(unlimitedStore, &service.Account{ID: 9, Platform: "kimi"})
		ctx, recorder := newCapHandlerTestContext(http.MethodPut, "/admin/accounts/9/concurrency-cap", `{"pinned":true}`, "9")
		unlimited.Update(ctx)
		require.Equal(t, http.StatusBadRequest, recorder.Code)
	})
}

func TestAdminConcurrencyCapHandlerWithoutStoreFailsClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var handler *AdminConcurrencyCapHandler

	ctx, recorder := newCapHandlerTestContext(http.MethodGet, "/admin/accounts/1/concurrency-cap", "", "1")
	handler.Get(ctx)

	require.Equal(t, http.StatusInternalServerError, recorder.Code)
	var payload response.Response
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &payload))
	require.NotEmpty(t, payload.Message)
}
