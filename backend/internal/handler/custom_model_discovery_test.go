package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/gemini"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type discoveryCustomRepository struct {
	service.CustomModelRepository
	models []service.CustomModel
	err    error
}

func (r *discoveryCustomRepository) List(_ context.Context, groupID int64) ([]service.CustomModel, error) {
	if r.err != nil {
		return nil, r.err
	}
	var result []service.CustomModel
	for _, model := range r.models {
		if !model.Enabled {
			continue
		}
		for _, bound := range model.DownstreamGroups {
			if bound == groupID || groupID == 0 {
				result = append(result, model)
				break
			}
		}
	}
	return result, nil
}

func (r *discoveryCustomRepository) Get(_ context.Context, id string) (*service.CustomModel, error) {
	if r.err != nil {
		return nil, r.err
	}
	for i := range r.models {
		if r.models[i].ModelID == id {
			return &r.models[i], nil
		}
	}
	return nil, service.ErrCustomModelNotFound
}

func discoveryCustomService(groupID int64) *service.CustomModelService {
	return service.NewCustomModelService(&discoveryCustomRepository{models: []service.CustomModel{
		{ModelID: "public-custom", Enabled: true, DownstreamGroups: []int64{groupID}},
		{ModelID: "disabled-custom", Enabled: false, DownstreamGroups: []int64{groupID}},
		{ModelID: "unbound-custom", Enabled: true, DownstreamGroups: []int64{groupID + 1}},
	}}, nil)
}

func TestCustomDiscoveryPreservesNativeCatalogues(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, platform := range []string{service.PlatformAnthropic, service.PlatformOpenAI, service.PlatformGemini, service.PlatformComposite, service.PlatformGrok} {
		t.Run(platform, func(t *testing.T) {
			group := &service.Group{ID: 741, Platform: platform}
			h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{})
			original := requestModelForTest(h, group, "", "")
			require.Equal(t, http.StatusOK, original.Code, original.Body.String())
			var native struct {
				Data []json.RawMessage `json:"data"`
			}
			require.NoError(t, json.Unmarshal(original.Body.Bytes(), &native))
			h.customModelService = discoveryCustomService(group.ID)
			listed := requestModelForTest(h, group, "", "")
			require.Equal(t, http.StatusOK, listed.Code, listed.Body.String())
			var merged struct {
				Data []json.RawMessage `json:"data"`
			}
			require.NoError(t, json.Unmarshal(listed.Body.Bytes(), &merged))
			require.Len(t, merged.Data, len(native.Data)+1)
			for i := range native.Data {
				require.JSONEq(t, string(native.Data[i]), string(merged.Data[i]))
			}
			retrieved := requestModelForTest(h, group, "public-custom", "")
			require.Equal(t, http.StatusOK, retrieved.Code, retrieved.Body.String())
			require.JSONEq(t, string(merged.Data[len(native.Data)]), retrieved.Body.String())
			for _, hidden := range []string{"disabled-custom", "unbound-custom"} {
				require.Equal(t, http.StatusNotFound, requestModelForTest(h, group, hidden, "").Code)
			}
			group.ModelAllowlist = service.GroupModelAllowlist{Enabled: true, Models: []string{"public-*"}}
			allowed := requestModelForTest(h, group, "", "")
			require.NoError(t, json.Unmarshal(allowed.Body.Bytes(), &merged))
			require.Len(t, merged.Data, 1)
			group.ModelAllowlist.Models = []string{"native-only"}
			require.Equal(t, http.StatusNotFound, requestModelForTest(h, group, "public-custom", "").Code)
			group.ID = 0
			group.ModelAllowlist.Enabled = false
			require.Equal(t, http.StatusNotFound, requestModelForTest(h, group, "public-custom", "").Code)
		})
	}
}

func TestCustomDiscoveryFailuresAreExplicit(t *testing.T) {
	svc := service.NewCustomModelService(&discoveryCustomRepository{err: errors.New("database unavailable")}, nil)
	group := &service.Group{ID: 742, Platform: service.PlatformOpenAI}
	h := &GatewayHandler{customModelService: svc}
	require.Equal(t, http.StatusInternalServerError, requestModelForTest(h, group, "", "").Code)
	codex := &OpenAIGatewayHandler{customModelService: svc}
	require.Equal(t, http.StatusInternalServerError, performCodexModelsRequestForGroup(t, codex, group, "").Code)
}

func TestCustomDiscoveryPinnedPreservesMetadataAndETags(t *testing.T) {
	group := &service.Group{ID: 743, Platform: service.PlatformOpenAI,
		CodexModelsManifestConfig: service.GroupCodexModelsManifestConfig{Enabled: true, AccountIDs: []int64{2}}}
	upstream := &codexModelsPinnedHTTPUpstream{bodies: map[int64]string{
		2: `{"data":[{"id":"native-model","owned_by":"native-owner","extra":{"context":123}}],"extra_envelope":true}`,
	}}
	codex := newPinnedCodexTestHandler([]service.Account{newPinnedCodexAccount(2, service.StatusActive, true, false)}, upstream, 3)
	h := &GatewayHandler{openAIGatewayService: codex.gatewayService, customModelService: discoveryCustomService(group.ID)}
	first := requestModelForTest(h, group, "", "")
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	require.Contains(t, first.Body.String(), `"extra":{"context":123}`)
	require.Contains(t, first.Body.String(), `"public-custom"`)
	require.Equal(t, http.StatusNotModified, requestModelForTest(h, group, "", first.Header().Get("ETag")).Code)
	require.Equal(t, http.StatusOK, requestModelForTest(h, group, "public-custom", first.Header().Get("ETag")).Code)
	h.customModelService = nil
	removed := requestModelForTest(h, group, "", first.Header().Get("ETag"))
	require.Equal(t, http.StatusOK, removed.Code, removed.Body.String())
	require.NotContains(t, removed.Body.String(), "public-custom")
}

func TestCustomDiscoveryGeminiNativeAndFallback(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		group := &service.Group{ID: 744, Platform: service.PlatformGemini}
		repo := &geminiAllowlistAccountRepoStub{gatewayModelsAccountRepoStub: gatewayModelsAccountRepoStub{byGroup: map[int64][]service.Account{
			group.ID: {{ID: 1, Platform: service.PlatformGemini, Type: service.AccountTypeAPIKey, Credentials: map[string]any{"api_key": "test"}}},
		}}}
		upstream := &geminiMixedModelsUpstream{status: http.StatusOK, body: `{"models":[{"name":"models/gemini-native","inputTokenLimit":123}],"nextPageToken":"next"}`}
		if fallback {
			upstream.status = http.StatusForbidden
			upstream.body = `{"error":"insufficient authentication scopes"}`
		}
		h := &GatewayHandler{
			customModelService:  discoveryCustomService(group.ID),
			geminiCompatService: service.NewGeminiMessagesCompatService(repo, nil, nil, nil, nil, nil, upstream, nil, &config.Config{}),
		}
		request := func(name string) *httptest.ResponseRecorder {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodGet, "/v1beta/models", nil)
			c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{GroupID: &group.ID, Group: group})
			if name != "" {
				c.Params = gin.Params{{Key: "model", Value: name}}
				h.GeminiV1BetaGetModel(c)
			} else {
				h.GeminiV1BetaListModels(c)
			}
			return rec
		}
		listed := request("")
		require.Equal(t, http.StatusOK, listed.Code, listed.Body.String())
		require.Contains(t, listed.Body.String(), `"models/public-custom"`)
		if fallback {
			for _, model := range gemini.DefaultModels() {
				require.Contains(t, listed.Body.String(), model.Name)
			}
		} else {
			require.Contains(t, listed.Body.String(), `"inputTokenLimit":123`)
			require.Contains(t, listed.Body.String(), `"nextPageToken":"next"`)
		}
		retrieved := request("public-custom")
		require.Equal(t, http.StatusOK, retrieved.Code, retrieved.Body.String())
		require.Contains(t, retrieved.Body.String(), `"name":"models/public-custom"`)
		for _, hidden := range []string{"disabled-custom", "unbound-custom"} {
			require.Equal(t, http.StatusNotFound, request(hidden).Code)
		}
		group.ModelAllowlist = service.GroupModelAllowlist{Enabled: true, Models: []string{"gemini-*"}}
		require.Equal(t, http.StatusNotFound, request("public-custom").Code)
		require.NotContains(t, request("").Body.String(), "public-custom")
	}
}

func TestCustomDiscoveryOfficialCodexKeepsNativeManifest(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		group := &service.Group{ID: 745, Platform: service.PlatformOpenAI,
			CodexModelsManifestConfig: service.GroupCodexModelsManifestConfig{Enabled: pinned, AccountIDs: []int64{2}}}
		account := newPinnedCodexAccount(2, service.StatusActive, true, false)
		upstream := &codexModelsPinnedHTTPUpstream{bodies: map[int64]string{
			2: `{"models":[{"slug":"native-model","display_name":"Native metadata","context_window":12345,"vendor_extension":true}],"native_envelope":true}`,
		}}
		h := newPinnedCodexTestHandler([]service.Account{account}, upstream, 3)
		h.customModelService = discoveryCustomService(group.ID)
		first := performCodexModelsRequestForGroup(t, h, group, "")
		require.Equal(t, http.StatusOK, first.Code, first.Body.String())
		require.Equal(t, []string{"native-model", "public-custom"}, codexHandlerManifestSlugs(t, first))
		require.Contains(t, first.Body.String(), `"vendor_extension":true`)
		require.Contains(t, first.Body.String(), `"native_envelope":true`)
		require.Equal(t, http.StatusNotModified, performCodexModelsRequestForGroup(t, h, group, first.Header().Get("ETag")).Code)
		group.ModelAllowlist = service.GroupModelAllowlist{Enabled: true, Models: []string{"public-custom"}}
		filtered := performCodexModelsRequestForGroup(t, h, group, first.Header().Get("ETag"))
		require.Equal(t, http.StatusOK, filtered.Code, filtered.Body.String())
		require.Equal(t, []string{"public-custom"}, codexHandlerManifestSlugs(t, filtered))
	}
}

func TestCustomDiscoveryGeminiResourceNames(t *testing.T) {
	for _, tc := range []struct {
		enabled    bool
		boundGroup int64
		wantStatus int
	}{
		{true, 746, http.StatusOK},
		{false, 746, http.StatusNotFound},
		{true, 747, http.StatusNotFound},
	} {
		group := &service.Group{ID: 746, Platform: service.PlatformGemini,
			ModelAllowlist: service.GroupModelAllowlist{Enabled: true, Models: []string{"public-custom"}}}
		h := &GatewayHandler{customModelService: service.NewCustomModelService(&discoveryCustomRepository{models: []service.CustomModel{
			{ModelID: "models/public-custom", Enabled: tc.enabled, DownstreamGroups: []int64{tc.boundGroup}},
		}}, nil)}
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodGet, "/v1beta/models/public-custom", nil)
		c.Params = gin.Params{{Key: "model", Value: "public-custom"}}
		c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{GroupID: &group.ID, Group: group})
		h.GeminiV1BetaGetModel(c)
		require.Equal(t, tc.wantStatus, rec.Code, rec.Body.String())
		if tc.wantStatus == http.StatusOK {
			require.JSONEq(t, `{"name":"models/public-custom","supportedGenerationMethods":["generateContent","streamGenerateContent"]}`, rec.Body.String())
		}
	}
}
