package controller

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"

	"github.com/gin-gonic/gin"

	"github.com/songquanpeng/one-api/common/client"
	"github.com/songquanpeng/one-api/common/ctxkey"
	"github.com/songquanpeng/one-api/middleware"
	"github.com/songquanpeng/one-api/model"
	"github.com/songquanpeng/one-api/relay"
	"github.com/songquanpeng/one-api/relay/apitype"
	"github.com/songquanpeng/one-api/relay/channeltype"
	"github.com/songquanpeng/one-api/relay/meta"
)

// channel types that are handled by the OpenAI adaptor with custom URL logic,
// for which GET /v1/models is not applicable
var channelTypeNoModelsEndpoint = map[int]bool{
	channeltype.Azure:                  true,
	channeltype.Minimax:                true,
	channeltype.Doubao:                 true,
	channeltype.Novita:                 true,
	channeltype.BaiduV2:                true,
	channeltype.AliBailian:             true,
	channeltype.GeminiOpenAICompatible: true,
}

func FetchChannelModels(c *gin.Context) {
	channel := model.Channel{}
	err := c.ShouldBindJSON(&channel)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	if channel.Id > 0 && channel.Key == "" {
		// the key is not exposed to the frontend when editing a channel, load it from db
		storedChannel, err := model.GetChannelById(channel.Id, true)
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": err.Error(),
			})
			return
		}
		channel.Key = storedChannel.Key
		if channel.GetBaseURL() == "" {
			channel.BaseURL = storedChannel.BaseURL
		}
		if channel.Config == "" {
			channel.Config = storedChannel.Config
		}
	}
	if channel.Key == "" {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "请填写渠道密钥后再获取模型",
		})
		return
	}
	if channelTypeNoModelsEndpoint[channel.Type] || channeltype.ToAPIType(channel.Type) != apitype.OpenAI {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "该渠道类型暂不支持自动获取模型",
		})
		return
	}
	modelIds, err := fetchModelsFromChannel(&channel)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    modelIds,
	})
}

func fetchModelsFromChannel(channel *model.Channel) ([]string, error) {
	w := httptest.NewRecorder()
	testCtx, _ := gin.CreateTestContext(w)
	testCtx.Request = &http.Request{
		Method: "GET",
		URL:    &url.URL{Path: "/v1/models"},
		Header: make(http.Header),
	}
	testCtx.Request.Header.Set("Authorization", "Bearer "+channel.Key)
	testCtx.Set(ctxkey.Channel, channel.Type)
	testCtx.Set(ctxkey.BaseURL, channel.GetBaseURL())
	cfg, _ := channel.LoadConfig()
	testCtx.Set(ctxkey.Config, cfg)
	middleware.SetupContextForSelectedChannel(testCtx, channel, "")
	relayMeta := meta.GetByContext(testCtx)
	apiType := channeltype.ToAPIType(channel.Type)
	adaptor := relay.GetAdaptor(apiType)
	if adaptor == nil {
		return nil, fmt.Errorf("invalid api type: %d", apiType)
	}
	adaptor.Init(relayMeta)
	requestURL, err := adaptor.GetRequestURL(relayMeta)
	if err != nil {
		return nil, fmt.Errorf("get request url failed: %w", err)
	}
	req, err := http.NewRequest("GET", requestURL, nil)
	if err != nil {
		return nil, err
	}
	err = adaptor.SetupRequestHeader(testCtx, req, relayMeta)
	if err != nil {
		return nil, err
	}
	resp, err := client.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		errMsg := string(body)
		if len(errMsg) > 512 {
			errMsg = errMsg[:512]
		}
		return nil, fmt.Errorf("获取模型失败，状态码 %d：%s", resp.StatusCode, errMsg)
	}
	var modelsResponse struct {
		Data []OpenAIModels `json:"data"`
	}
	err = json.Unmarshal(body, &modelsResponse)
	if err != nil {
		return nil, fmt.Errorf("解析模型列表失败：%w", err)
	}
	modelIds := make([]string, 0, len(modelsResponse.Data))
	for _, m := range modelsResponse.Data {
		if m.Id != "" {
			modelIds = append(modelIds, m.Id)
		}
	}
	if len(modelIds) == 0 {
		return nil, fmt.Errorf("上游未返回任何模型")
	}
	return modelIds, nil
}
