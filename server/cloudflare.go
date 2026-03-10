// Copyright (c) 2020-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/mattermost/mattermost-plugin-calls/server/public"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/rtcd/service/rtc"
)

const (
	cloudflareAPIBaseURL = "https://rtc.live.cloudflare.com/v1/apps"
)

// isCloudflareBackend は Cloudflare Calls バックエンドが使用されているかどうかを返す。
// rtcServer も rtcdManager も nil の場合は Cloudflare バックエンドが選択されている。
func (p *Plugin) isCloudflareBackend() bool {
	cfg := p.getConfiguration()
	return cfg != nil && cfg.CloudflareCallsAppID != "" && cfg.CloudflareCallsAppToken != ""
}

func (p *Plugin) cloudflareAPIBase() (string, error) {
	cfg := p.getConfiguration()
	if cfg == nil || cfg.CloudflareCallsAppID == "" {
		return "", fmt.Errorf("CloudflareCallsAppID is not configured")
	}
	return cloudflareAPIBaseURL + "/" + cfg.CloudflareCallsAppID, nil
}

func (p *Plugin) cloudflareAuthHeader() (string, error) {
	cfg := p.getConfiguration()
	if cfg == nil || cfg.CloudflareCallsAppToken == "" {
		return "", fmt.Errorf("CloudflareCallsAppToken is not configured")
	}
	return "Bearer " + cfg.CloudflareCallsAppToken, nil
}

func (p *Plugin) handleSdpMessage(msg rtc.Message, callID string) error {
	apiBase, err := p.cloudflareAPIBase()
	if err != nil {
		return fmt.Errorf("cloudflare not configured: %w", err)
	}
	authHeader, err := p.cloudflareAuthHeader()
	if err != nil {
		return fmt.Errorf("cloudflare not configured: %w", err)
	}

	client := &http.Client{}

	// POST /apps/{appId}/sessions/new でセッションを新規作成
	req, err := http.NewRequest("POST", apiBase+"/sessions/new", nil)
	if err != nil {
		return fmt.Errorf("failed to create new session request: %w", err)
	}
	req.Header.Add("Authorization", authHeader)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute new session request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("unexpected status %d from new session: %s", resp.StatusCode, string(bodyBytes))
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read new session response body: %w", err)
	}

	var newSessionResp map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &newSessionResp); err != nil {
		return fmt.Errorf("failed to unmarshal new session response: %w", err)
	}

	cfSessionID, ok := newSessionResp["sessionId"].(string)
	if !ok || cfSessionID == "" {
		return fmt.Errorf("sessionId not found in new session response")
	}

	// msg.Data は json.Marshal({sdp: []byte, tracks: []}) の形式
	var dataMap map[string]interface{}
	if err := json.Unmarshal(msg.Data, &dataMap); err != nil {
		return fmt.Errorf("failed to unmarshal sdp message data: %w", err)
	}

	// sdp フィールドは base64 エンコードされた SDP JSON バイト列
	sdpBase64, ok := dataMap["sdp"].(string)
	if !ok {
		return fmt.Errorf("invalid or missing 'sdp' field in message data")
	}

	sdpJSONBytes, err := base64.StdEncoding.DecodeString(sdpBase64)
	if err != nil {
		return fmt.Errorf("failed to decode base64 sdp: %w", err)
	}

	var sdpObj map[string]interface{}
	if err := json.Unmarshal(sdpJSONBytes, &sdpObj); err != nil {
		return fmt.Errorf("failed to unmarshal sdp json: %w", err)
	}

	sdpStr, _ := sdpObj["sdp"].(string)
	if sdpStr == "" {
		return fmt.Errorf("missing 'sdp' field in SDP JSON object")
	}

	tracks, ok := dataMap["tracks"].([]interface{})
	if !ok {
		return fmt.Errorf("invalid or missing 'tracks' field in message data")
	}

	trackList := make([]map[string]interface{}, 0, len(tracks))
	for _, track := range tracks {
		trackMap, ok := track.(map[string]interface{})
		if !ok {
			return fmt.Errorf("invalid track entry: expected map, got %T", track)
		}
		location, _ := trackMap["location"].(string)
		mid, _ := trackMap["mid"].(string)
		trackName, _ := trackMap["trackName"].(string)
		trackList = append(trackList, map[string]interface{}{
			"location":  location,
			"mid":       mid,
			"trackName": trackName,
		})
	}

	// STEP 1: 自分のトラックを push（tracks/new with offer）
	tracksReqBody := map[string]interface{}{
		"sessionDescription": map[string]interface{}{
			"type": "offer",
			"sdp":  sdpStr,
		},
		"tracks": trackList,
	}

	jsonBody, err := json.Marshal(tracksReqBody)
	if err != nil {
		return fmt.Errorf("failed to marshal tracks request: %w", err)
	}

	req, err = http.NewRequest("POST", apiBase+"/sessions/"+cfSessionID+"/tracks/new", bytes.NewReader(jsonBody))
	if err != nil {
		return fmt.Errorf("failed to create tracks/new request: %w", err)
	}
	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("Authorization", authHeader)

	resp, err = client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute tracks/new request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("unexpected status %d from tracks/new: %s", resp.StatusCode, string(bodyBytes))
	}

	bodyBytes, err = io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read tracks/new response body: %w", err)
	}

	// push レスポンスから Cloudflare が割り当てた trackId を取得する
	var pushResp map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &pushResp); err != nil {
		return fmt.Errorf("failed to unmarshal push response: %w", err)
	}

	// Cloudflare が割り当てたトラック ID リスト (tracks[].trackName) を取得
	var pushedTrackNames []string
	if respTracks, ok := pushResp["tracks"].([]interface{}); ok {
		for _, rt := range respTracks {
			if rtMap, ok := rt.(map[string]interface{}); ok {
				if tn, ok := rtMap["trackName"].(string); ok && tn != "" {
					pushedTrackNames = append(pushedTrackNames, tn)
				}
			}
		}
	}

	// Cloudflareセッション情報をDBに保存（MMセッションID → Cloudflare SessionID マッピング）
	cloudflareSession := &public.CallCloudflareSession{
		ID:                      model.NewId(),
		CallID:                  callID,
		MMSessionID:             msg.SessionID,
		CloudflareCallSessionID: cfSessionID,
	}
	if err := p.store.CreateCallCloudflareSession(cloudflareSession); err != nil {
		return fmt.Errorf("failed to save cloudflare session: %w", err)
	}

	// Cloudflare API レスポンス (answer) を新規参加者クライアントへ送信
	us := p.getSessionByOriginalID(msg.SessionID)
	if us == nil {
		return fmt.Errorf("session not found for originalConnID: %s", msg.SessionID)
	}
	p.publishWebSocketEvent(wsEventSignal, map[string]interface{}{
		"data":   string(bodyBytes),
		"connID": msg.SessionID,
	}, &WebSocketBroadcast{ConnectionID: us.connID, ReliableClusterSend: true})

	// STEP 2: 既存参加者のトラックをこのセッションに pull させる
	// 同じ通話の既存 Cloudflare セッションを取得
	existingSessions, err := p.store.GetCallCloudflareSessions(callID)
	if err != nil {
		p.LogError("failed to get existing cloudflare sessions", "err", err.Error(), "callID", callID)
	} else {
		var pullTracks []map[string]interface{}
		for _, existingSession := range existingSessions {
			if existingSession.MMSessionID == msg.SessionID {
				continue // 自分自身はスキップ
			}
			// 既存セッションの Cloudflare API から公開トラック一覧を取得
			existingTracksFromCF, err := p.getCloudflareSessionTracks(apiBase, authHeader, existingSession.CloudflareCallSessionID)
			if err != nil {
				p.LogError("failed to get tracks for existing session", "err", err.Error(), "cfSessionID", existingSession.CloudflareCallSessionID)
				continue
			}
			for _, trackName := range existingTracksFromCF {
				pullTracks = append(pullTracks, map[string]interface{}{
					"location":  "remote",
					"sessionId": existingSession.CloudflareCallSessionID,
					"trackName": trackName,
				})
			}
		}

		if len(pullTracks) > 0 {
			// 新規参加者セッションに既存トラックを pull
			if err := p.pullTracksForSession(apiBase, authHeader, cfSessionID, pullTracks, us); err != nil {
				p.LogError("failed to pull existing tracks for new session", "err", err.Error())
			}
		}
	}

	// STEP 3: 既存参加者全員に新規参加者のトラックを pull させる
	if len(pushedTrackNames) > 0 {
		existingSessions2, err := p.store.GetCallCloudflareSessions(callID)
		if err != nil {
			p.LogError("failed to get existing cloudflare sessions for notify", "err", err.Error())
		} else {
			for _, existingSession := range existingSessions2 {
				if existingSession.MMSessionID == msg.SessionID {
					continue // 自分自身はスキップ
				}
				existingUS := p.getSessionByOriginalID(existingSession.MMSessionID)
				if existingUS == nil {
					continue
				}
				newTracks := make([]map[string]interface{}, 0, len(pushedTrackNames))
				for _, trackName := range pushedTrackNames {
					newTracks = append(newTracks, map[string]interface{}{
						"location":  "remote",
						"sessionId": cfSessionID,
						"trackName": trackName,
					})
				}
				if err := p.pullTracksForSession(apiBase, authHeader, existingSession.CloudflareCallSessionID, newTracks, existingUS); err != nil {
					p.LogError("failed to pull new tracks for existing session", "err", err.Error(), "mmSessionID", existingSession.MMSessionID)
				}
			}
		}
	}

	return nil
}

// getCloudflareSessionTracks は指定 CF セッションが持つ push 済みトラックの trackName 一覧を返す
func (p *Plugin) getCloudflareSessionTracks(apiBase, authHeader, cfSessionID string) ([]string, error) {
	req, err := http.NewRequest("GET", apiBase+"/sessions/"+cfSessionID, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create get session request: %w", err)
	}
	req.Header.Add("Authorization", authHeader)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute get session request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("unexpected status %d from get session: %s", resp.StatusCode, string(bodyBytes))
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read get session response: %w", err)
	}

	var sessionInfo map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &sessionInfo); err != nil {
		return nil, fmt.Errorf("failed to unmarshal session info: %w", err)
	}

	var trackNames []string
	if tracksRaw, ok := sessionInfo["tracks"].([]interface{}); ok {
		for _, t := range tracksRaw {
			if tMap, ok := t.(map[string]interface{}); ok {
				// location == "local" のトラックのみ（push 済み）
				if loc, _ := tMap["location"].(string); loc == "local" {
					if tn, ok := tMap["trackName"].(string); ok && tn != "" {
						trackNames = append(trackNames, tn)
					}
				}
			}
		}
	}
	return trackNames, nil
}

// pullTracksForSession は指定 CF セッションに remote トラックを追加し、renegotiate を行い、
// answer を us（クライアント）に wsEventSignal で送信する
func (p *Plugin) pullTracksForSession(apiBase, authHeader, cfSessionID string, pullTracks []map[string]interface{}, us *session) error {
	pullReqBody := map[string]interface{}{
		"tracks": pullTracks,
	}
	jsonBody, err := json.Marshal(pullReqBody)
	if err != nil {
		return fmt.Errorf("failed to marshal pull request: %w", err)
	}

	client := &http.Client{}
	req, err := http.NewRequest("POST", apiBase+"/sessions/"+cfSessionID+"/tracks/new", bytes.NewReader(jsonBody))
	if err != nil {
		return fmt.Errorf("failed to create pull tracks request: %w", err)
	}
	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("Authorization", authHeader)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute pull tracks request: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read pull tracks response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d from pull tracks: %s", resp.StatusCode, string(bodyBytes))
	}

	// Cloudflare は pull の場合 offer を返す → クライアントが answer を作り renegotiate する
	// wsEventSignal でクライアントに offer を送信する
	p.publishWebSocketEvent(wsEventSignal, map[string]interface{}{
		"data":   string(bodyBytes),
		"connID": us.originalConnID,
	}, &WebSocketBroadcast{ConnectionID: us.connID, ReliableClusterSend: true})

	return nil
}

func (p *Plugin) handleIceMessage(mmSessionID string, data []byte) error {
	apiBase, err := p.cloudflareAPIBase()
	if err != nil {
		return fmt.Errorf("cloudflare not configured: %w", err)
	}
	authHeader, err := p.cloudflareAuthHeader()
	if err != nil {
		return fmt.Errorf("cloudflare not configured: %w", err)
	}

	// ICE candidate を Cloudflare Calls API に転送する
	cfSession, err := p.store.GetCallCloudflareSession(mmSessionID)
	if err != nil {
		return nil
	}

	// data = JSON 文字列の ICE candidate
	// Cloudflare Calls API: PUT /apps/{appId}/sessions/{sessionId}/ice
	req, err := http.NewRequest("PUT",
		apiBase+"/sessions/"+cfSession.CloudflareCallSessionID+"/ice",
		bytes.NewReader(data),
	)
	if err != nil {
		return fmt.Errorf("failed to create ICE request: %w", err)
	}
	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("Authorization", authHeader)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute ICE request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("unexpected status %d from ICE PUT: %s", resp.StatusCode, string(bodyBytes))
	}

	return nil
}

// handleRenegotiateMessage はクライアントから送られた answer (renegotiate) を
// Cloudflare Calls API の /sessions/{sessionId}/renegotiate エンドポイントに転送する
func (p *Plugin) handleRenegotiateMessage(mmSessionID string, data []byte) error {
	apiBase, err := p.cloudflareAPIBase()
	if err != nil {
		return fmt.Errorf("cloudflare not configured: %w", err)
	}
	authHeader, err := p.cloudflareAuthHeader()
	if err != nil {
		return fmt.Errorf("cloudflare not configured: %w", err)
	}

	cfSession, err := p.store.GetCallCloudflareSession(mmSessionID)
	if err != nil {
		return fmt.Errorf("failed to get cloudflare session for renegotiate: %w", err)
	}

	// data = {"sdp": {"type":"answer","sdp":"v=0..."}} 形式
	// unpackSDPData で解凍済みの JSON オブジェクトが入っている
	var dataMap map[string]interface{}
	if err := json.Unmarshal(data, &dataMap); err != nil {
		return fmt.Errorf("failed to unmarshal renegotiate data: %w", err)
	}

	// sdp フィールドは {"type":"answer","sdp":"v=0..."} 形式の map
	var sdpStr string
	switch v := dataMap["sdp"].(type) {
	case map[string]interface{}:
		// 正常ケース: {"type":"answer","sdp":"v=0..."}
		sdpStr, _ = v["sdp"].(string)
	case string:
		// フォールバック: SDP 文字列がそのまま入っている場合
		sdpStr = v
	}

	if sdpStr == "" {
		return fmt.Errorf("missing sdp in renegotiate message")
	}

	reqBody := map[string]interface{}{
		"sessionDescription": map[string]interface{}{
			"type": "answer",
			"sdp":  sdpStr,
		},
	}
	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("failed to marshal renegotiate request: %w", err)
	}

	req, err := http.NewRequest("PUT",
		apiBase+"/sessions/"+cfSession.CloudflareCallSessionID+"/renegotiate",
		bytes.NewReader(jsonBody),
	)
	if err != nil {
		return fmt.Errorf("failed to create renegotiate request: %w", err)
	}
	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("Authorization", authHeader)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute renegotiate request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d from renegotiate: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

func (p *Plugin) handleAddUser(msg rtc.Message, callID string) error {
	apiBase, err := p.cloudflareAPIBase()
	if err != nil {
		return fmt.Errorf("cloudflare not configured: %w", err)
	}
	authHeader, err := p.cloudflareAuthHeader()
	if err != nil {
		return fmt.Errorf("cloudflare not configured: %w", err)
	}

	// msg.Data は {"tracks": [...]} 形式のJSONエンコード済みデータ
	var dataMap map[string]interface{}
	if err := json.Unmarshal(msg.Data, &dataMap); err != nil {
		return fmt.Errorf("failed to unmarshal add_user message data: %w", err)
	}

	tracks, ok := dataMap["tracks"].([]interface{})
	if !ok {
		return fmt.Errorf("invalid or missing 'tracks' field in add_user message")
	}

	trackList := make([]map[string]interface{}, 0, len(tracks))
	for _, track := range tracks {
		trackMap, ok := track.(map[string]interface{})
		if !ok {
			return fmt.Errorf("invalid track entry: expected map, got %T", track)
		}
		location, _ := trackMap["location"].(string)
		mid, _ := trackMap["mid"].(string)
		trackName, _ := trackMap["trackName"].(string)
		trackList = append(trackList, map[string]interface{}{
			"location":  location,
			"mid":       mid,
			"trackName": trackName,
		})
	}

	reqBody := map[string]interface{}{
		"tracks": trackList,
	}
	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("failed to marshal add_user request: %w", err)
	}

	// DBからMMセッションIDに対応するCloudflareセッションIDを取得
	cfSession, err := p.store.GetCallCloudflareSession(msg.SessionID)
	if err != nil {
		return fmt.Errorf("failed to get cloudflare session for mmSessionID %s: %w", msg.SessionID, err)
	}

	client := &http.Client{}
	req, err := http.NewRequest("POST",
		apiBase+"/sessions/"+cfSession.CloudflareCallSessionID+"/tracks/new",
		bytes.NewReader(jsonBody),
	)
	if err != nil {
		return fmt.Errorf("failed to create add_user tracks/new request: %w", err)
	}
	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("Authorization", authHeader)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute add_user tracks/new request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("unexpected status %d from add_user tracks/new: %s", resp.StatusCode, string(bodyBytes))
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read add_user response body: %w", err)
	}

	// answerをクライアントへ送信
	us := p.getSessionByOriginalID(msg.SessionID)
	if us == nil {
		return fmt.Errorf("session not found for originalConnID: %s", msg.SessionID)
	}
	p.publishWebSocketEvent(wsEventSignal, map[string]interface{}{
		"data":   string(bodyBytes),
		"connID": msg.SessionID,
	}, &WebSocketBroadcast{ConnectionID: us.connID, ReliableClusterSend: true})

	return nil
}
