package wire

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// tiny valid payloads — enough to exercise the validator without embedding a
// real screenshot in the test source
var (
	pngB64  = base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\n fake png bytes"))
	jpegB64 = base64.StdEncoding.EncodeToString([]byte("\xff\xd8\xff\xe0 fake jpeg bytes"))
	pngURL  = "data:image/png;base64," + pngB64
	jpegURL = "data:image/jpeg;base64," + jpegB64
)

// decodeAlpha unmarshals a built envelope back into a generic form so tests
// assert on the bytes that actually go on the wire.
func decodeAlpha(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("envelope is not valid JSON: %v", err)
	}
	return m
}

func alphaMessages(t *testing.T, body []byte) []any {
	t.Helper()
	params, ok := decodeAlpha(t, body)["params"].(map[string]any)
	if !ok {
		t.Fatal("envelope has no params object")
	}
	msgs, ok := params["messages"].([]any)
	if !ok {
		t.Fatal("envelope has no messages array")
	}
	return msgs
}

func partsOf(t *testing.T, msg any) []map[string]any {
	t.Helper()
	m, ok := msg.(map[string]any)
	if !ok {
		t.Fatalf("message is not an object: %T", msg)
	}
	raw, ok := m["content"].([]any)
	if !ok {
		t.Fatalf("message content is not an array: %T", m["content"])
	}
	out := make([]map[string]any, 0, len(raw))
	for _, p := range raw {
		pm, ok := p.(map[string]any)
		if !ok {
			t.Fatalf("content part is not an object: %T", p)
		}
		out = append(out, pm)
	}
	return out
}

// (1) OpenAI image_url request → normalized Msg.Images
func TestOpenAIImageURLNormalizes(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":[
		{"type":"text","text":"what is this?"},
		{"type":"image_url","image_url":{"url":"` + pngURL + `"}},
		{"type":"input_image","image_url":"` + jpegURL + `"}]}]}`)
	r, err := ParseOpenAIRequest(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(r.Messages) != 1 {
		t.Fatalf("want 1 message, got %d", len(r.Messages))
	}
	m := r.Messages[0]
	if m.Content != "what is this?" {
		t.Errorf("text lost: %q", m.Content)
	}
	if len(m.Images) != 2 {
		t.Fatalf("want 2 images, got %d", len(m.Images))
	}
	if m.Images[0] != pngURL || m.Images[1] != jpegURL {
		t.Error("image order or content not preserved")
	}
}

// (2)(3) normalized message → alpha image part, text-then-image ordering
func TestAlphaImagePartAndOrdering(t *testing.T) {
	body, err := BuildCommandCodeRequest(&Request{
		Model:    "deepseek/deepseek-v4-flash-vision-exp",
		Messages: []Msg{{Role: "user", Content: "describe it", Images: []string{pngURL}}},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	parts := partsOf(t, alphaMessages(t, body)[0])
	if len(parts) != 2 {
		t.Fatalf("want text+image parts, got %d", len(parts))
	}
	if parts[0]["type"] != "text" || parts[0]["text"] != "describe it" {
		t.Errorf("part 0 should be the text: %v", parts[0])
	}
	if parts[1]["type"] != "image" {
		t.Fatalf("part 1 should be the image: %v", parts[1])
	}
	if parts[1]["image"] != pngURL {
		t.Error("image payload not carried verbatim")
	}
	if parts[1]["mediaType"] != "image/png" {
		t.Errorf("mediaType = %v, want image/png", parts[1]["mediaType"])
	}
	// the shapes alpha rejects must never appear
	if _, bad := parts[1]["url"]; bad {
		t.Error(`emitted a "url" field; alpha 400s on that shape`)
	}
	if parts[1]["type"] == "image_url" || parts[1]["type"] == "file" {
		t.Error("emitted a rejected part type")
	}
}

// (4) multiple images in one message, order preserved
func TestAlphaMultipleImages(t *testing.T) {
	third := "data:image/webp;base64," + base64.StdEncoding.EncodeToString([]byte("webp bytes"))
	body, err := BuildCommandCodeRequest(&Request{
		Model:    "m",
		Messages: []Msg{{Role: "user", Content: "compare", Images: []string{pngURL, jpegURL, third}}},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	parts := partsOf(t, alphaMessages(t, body)[0])
	if len(parts) != 4 {
		t.Fatalf("want text + 3 images, got %d parts", len(parts))
	}
	want := []string{pngURL, jpegURL, third}
	for i, w := range want {
		if parts[i+1]["type"] != "image" || parts[i+1]["image"] != w {
			t.Errorf("image %d out of order or altered", i+1)
		}
	}
	if parts[1]["mediaType"] != "image/png" || parts[2]["mediaType"] != "image/jpeg" || parts[3]["mediaType"] != "image/webp" {
		t.Error("media types not carried per-image")
	}
}

// (5)(6) inline PNG and JPEG both survive
func TestAlphaInlinePNGAndJPEG(t *testing.T) {
	for _, tc := range []struct{ name, url, media string }{
		{"png", pngURL, "image/png"},
		{"jpeg", jpegURL, "image/jpeg"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := BuildCommandCodeRequest(&Request{Model: "m",
				Messages: []Msg{{Role: "user", Images: []string{tc.url}}}})
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			parts := partsOf(t, alphaMessages(t, body)[0])
			if parts[0]["type"] != "image" || parts[0]["mediaType"] != tc.media {
				t.Errorf("got %v, want an %s image part", parts[0], tc.media)
			}
		})
	}
}

// (7) https URL — schema-valid upstream, so it is passed through unchanged
func TestAlphaHTTPSImageURL(t *testing.T) {
	const u = "https://example.com/plan.png"
	body, err := BuildCommandCodeRequest(&Request{Model: "m",
		Messages: []Msg{{Role: "user", Content: "read it", Images: []string{u}}}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	parts := partsOf(t, alphaMessages(t, body)[0])
	if parts[1]["type"] != "image" || parts[1]["image"] != u {
		t.Fatalf("https URL not passed through: %v", parts[1])
	}
	if _, ok := parts[1]["mediaType"]; ok {
		t.Error("mediaType guessed for a remote URL; it is unknown until fetched")
	}
}

// (8) malformed data URLs are rejected, never sent
func TestAlphaMalformedDataURLRejected(t *testing.T) {
	bad := map[string]string{
		"no comma":        "data:image/png;base64" + strings.Repeat("A", 40),
		"not base64":      "data:image/png;base64,####not base64####",
		"empty payload":   "data:image/png;base64,",
		"not an image":    "data:application/pdf;base64," + pngB64,
		"missing base64":  "data:image/png,rawbytes",
		"truncated b64":   "data:image/png;base64,QQQQQ",
		"padding in mid":  "data:image/png;base64,QQ==QQ",
		"empty reference": "",
		"file scheme":     "file:///etc/passwd",
		"bare base64":     pngB64,
	}
	for name, img := range bad {
		t.Run(name, func(t *testing.T) {
			body, err := BuildCommandCodeRequest(&Request{Model: "m",
				Messages: []Msg{{Role: "user", Content: "hi", Images: []string{img}}}})
			if err == nil {
				t.Fatalf("accepted a malformed image; body would have gone out: %d bytes", len(body))
			}
			if strings.Contains(err.Error(), pngB64) || (img != "" && len(img) > 60 && strings.Contains(err.Error(), img[:60])) {
				t.Error("error message leaks image payload")
			}
		})
	}
}

// (9) an image can never be silently dropped
func TestAlphaImageNeverSilentlyDropped(t *testing.T) {
	// an image on a non-user turn cannot be represented: alpha accepts image
	// parts on user messages only
	if _, err := BuildCommandCodeRequest(&Request{Model: "m", Messages: []Msg{
		{Role: "assistant", Content: "here", Images: []string{pngURL}},
	}}); err == nil {
		t.Fatal("assistant-role image was accepted; upstream rejects that shape")
	}
	// and the counting guard catches any future path that loses one
	msgs := []Msg{{Role: "user", Content: "a", Images: []string{pngURL, jpegURL}}}
	body, err := BuildCommandCodeRequest(&Request{Model: "m", Messages: msgs})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var got int
	for _, m := range alphaMessages(t, body) {
		for _, p := range partsOf(t, m) {
			if p["type"] == "image" {
				got++
			}
		}
	}
	if got != countRequestImages(msgs) {
		t.Fatalf("guard let %d of %d images through", got, countRequestImages(msgs))
	}
}

// (10) tool schema survives translation — SheetIntel-shaped nested schema
func TestAlphaNestedToolSchemaSurvives(t *testing.T) {
	schema := json.RawMessage(`{
	  "type":"object",
	  "properties":{
	    "sheet":{"type":"object","properties":{
	      "number":{"type":"string"},
	      "title":{"type":"string"},
	      "discipline":{"type":"string","enum":["A","S","M","E","P"]},
	      "titleblock":{"type":"object","properties":{
	        "project":{"type":"string"},"date":{"type":"string"},
	        "revisions":{"type":"array","items":{"type":"object","properties":{
	          "rev":{"type":"string"},"desc":{"type":"string"}},"required":["rev"]}}},
	        "required":["project"]}},
	      "required":["number","title","titleblock"]},
	    "findings":{"type":"array","items":{"type":"object","properties":{
	      "id":{"type":"string"},
	      "severity":{"type":"string","enum":["info","low","medium","high"]},
	      "location":{"type":"object","properties":{
	        "grid":{"type":"string"},"detail":{"type":"string"}}},
	      "evidence":{"type":"array","items":{"type":"string"}}},
	      "required":["id","severity"]}},
	    "confidence":{"type":"number"}},
	  "required":["sheet","findings"]}`)
	body, err := BuildCommandCodeRequest(&Request{
		Model:    "m",
		Messages: []Msg{{Role: "user", Content: "review it"}},
		Tools:    []Tool{{Name: "submit_sheet_review", Description: "Submit a structured review", Params: schema}},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	params := decodeAlpha(t, body)["params"].(map[string]any)
	tools, ok := params["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools not carried: %v", params["tools"])
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "submit_sheet_review" {
		t.Errorf("tool name lost: %v", tool["name"])
	}
	// the schema must arrive structurally identical, not stringified or flattened
	var want, got any
	if err := json.Unmarshal(schema, &want); err != nil {
		t.Fatal(err)
	}
	roundTrip, err := json.Marshal(tool["input_schema"])
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(roundTrip, &got); err != nil {
		t.Fatal(err)
	}
	wantJSON, _ := json.Marshal(want)
	gotJSON, _ := json.Marshal(got)
	if string(wantJSON) != string(gotJSON) {
		t.Errorf("nested schema altered in translation:\n want %s\n got  %s", wantJSON, gotJSON)
	}
}

// (11) streamed tool arguments reconstruct into valid JSON
func TestAlphaStreamedToolArgsReconstruct(t *testing.T) {
	full := `{"sheet":{"number":"A-101","title":"First Floor Plan","titleblock":{"project":"Museum","revisions":[{"rev":"2","desc":"issued"}]}},"findings":[{"id":"f1","severity":"high","evidence":["grid B-4"]}],"confidence":0.82}`
	var ndjson strings.Builder
	ndjson.WriteString(`{"type":"tool-input-start","id":"call_1","toolName":"submit_sheet_review"}` + "\n")
	for i := 0; i < len(full); i += 17 { // deliberately mid-token fragments
		end := i + 17
		if end > len(full) {
			end = len(full)
		}
		frag, _ := json.Marshal(full[i:end])
		ndjson.WriteString(`{"type":"tool-input-delta","id":"call_1","delta":` + string(frag) + "}\n")
	}
	ndjson.WriteString(`{"type":"finish","finishReason":"tool-calls","usage":{"inputTokens":10,"outputTokens":5,"cachedInputTokens":2}}` + "\n")

	// buffered path
	resp, err := ParseCommandCodeResponse([]byte(ndjson.String()))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("want 1 tool call, got %d", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].Name != "submit_sheet_review" {
		t.Errorf("tool name lost: %q", resp.ToolCalls[0].Name)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(resp.ToolCalls[0].Args), &parsed); err != nil {
		t.Fatalf("reconstructed args are not valid JSON: %v", err)
	}
	if resp.FinishReason != "tool_calls" {
		t.Errorf("finish = %q, want tool_calls", resp.FinishReason)
	}
	// streaming path reassembles to exactly the same JSON
	ch := make(chan Delta, 256)
	go ReadCommandCodeStream(strings.NewReader(ndjson.String()), ch)
	var args strings.Builder
	for d := range ch {
		if d.Err != nil {
			t.Fatalf("stream error: %v", d.Err)
		}
		if d.TC != nil {
			args.WriteString(d.TC.Args)
		}
	}
	if args.String() != full {
		t.Errorf("streamed args differ from source:\n got  %s\n want %s", args.String(), full)
	}
	if err := json.Unmarshal([]byte(args.String()), &parsed); err != nil {
		t.Fatalf("streamed args are not valid JSON: %v", err)
	}
}

// tool_choice maps only onto shapes the live protocol accepts
func TestAlphaToolChoiceMapping(t *testing.T) {
	tools := []Tool{{Name: "submit_sheet_review", Params: json.RawMessage(`{"type":"object"}`)}}
	choice := func(raw string) any {
		body, err := BuildCommandCodeRequest(&Request{Model: "m",
			Messages: []Msg{{Role: "user", Content: "x"}}, Tools: tools,
			ToolChoice: json.RawMessage(raw)})
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		return decodeAlpha(t, body)["params"].(map[string]any)["tool_choice"]
	}
	if got := choice(`"auto"`); got == nil || got.(map[string]any)["type"] != "auto" {
		t.Errorf(`"auto" -> %v`, got)
	}
	if got := choice(`"required"`); got == nil || got.(map[string]any)["type"] != "any" {
		t.Errorf(`"required" -> %v, want {"type":"any"} (alpha rejects "required")`, got)
	}
	if got := choice(`{"type":"function","function":{"name":"submit_sheet_review"}}`); got == nil ||
		got.(map[string]any)["type"] != "tool" || got.(map[string]any)["name"] != "submit_sheet_review" {
		t.Errorf(`function choice -> %v, want {"type":"tool","name":…}`, got)
	}
	// "none" has no alpha spelling: omit rather than invent one
	if got := choice(`"none"`); got != nil {
		t.Errorf(`"none" -> %v, want omitted`, got)
	}
	// no tools declared: nothing to choose
	body, err := BuildCommandCodeRequest(&Request{Model: "m",
		Messages: []Msg{{Role: "user", Content: "x"}}, ToolChoice: json.RawMessage(`"required"`)})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, present := decodeAlpha(t, body)["params"].(map[string]any)["tool_choice"]; present {
		t.Error("tool_choice emitted with no tools declared")
	}
}

// (12 guard) text-only and tool-call conversations are untouched by the change
func TestAlphaTextAndToolTurnsUnchanged(t *testing.T) {
	body, err := BuildCommandCodeRequest(&Request{
		Model:  "m",
		System: "be brief",
		Messages: []Msg{
			{Role: "user", Content: "call the tool"},
			{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "grep", Args: `{"q":"x"}`}}},
			{Role: "tool", ToolCallID: "c1", Content: "3 hits"},
		},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	msgs := alphaMessages(t, body)
	if len(msgs) != 3 {
		t.Fatalf("want 3 messages, got %d", len(msgs))
	}
	if p := partsOf(t, msgs[0]); p[0]["type"] != "text" || p[0]["text"] != "call the tool" {
		t.Errorf("user text turn changed: %v", p)
	}
	p1 := partsOf(t, msgs[1])
	if p1[0]["type"] != "tool-call" || p1[0]["toolName"] != "grep" {
		t.Errorf("assistant tool-call turn changed: %v", p1)
	}
	p2 := partsOf(t, msgs[2])
	if p2[0]["type"] != "tool-result" || p2[0]["toolName"] != "grep" {
		t.Errorf("tool-result turn changed: %v", p2)
	}
	out := p2[0]["output"].(map[string]any)
	if out["type"] != "text" || out["value"] != "3 hits" {
		t.Errorf("tool output shape changed: %v", out)
	}
	if _, ok := decodeAlpha(t, body)["params"].(map[string]any)["tool_choice"]; ok {
		t.Error("tool_choice appeared on a request that never asked for one")
	}
}

// End-to-end through the normalized form: an OpenAI vision request with a
// structured-output tool becomes an alpha envelope carrying both the image and
// the schema, and the streamed reply comes back as a normal OpenAI tool call.
// This is the whole SheetIntel path in one test.
func TestOpenAIVisionToolRoundTripThroughAlpha(t *testing.T) {
	in := []byte(`{"model":"commandcode/deepseek/deepseek-v4-flash-vision-exp",
	  "messages":[{"role":"user","content":[
	    {"type":"text","text":"Read the title block."},
	    {"type":"image_url","image_url":{"url":"` + pngURL + `"}}]}],
	  "tools":[{"type":"function","function":{"name":"submit_sheet_review",
	    "description":"structured review",
	    "parameters":{"type":"object","properties":{
	      "sheet_number":{"type":"string"},
	      "findings":{"type":"array","items":{"type":"object",
	        "properties":{"severity":{"type":"string"},"note":{"type":"string"}},
	        "required":["severity","note"]}}},
	      "required":["sheet_number","findings"]}}}],
	  "tool_choice":{"type":"function","function":{"name":"submit_sheet_review"}}}`)

	r, err := ParseOpenAIRequest(in)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	body, err := BuildCommandCodeRequest(r)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	params := decodeAlpha(t, body)["params"].(map[string]any)

	parts := partsOf(t, alphaMessages(t, body)[0])
	if len(parts) != 2 || parts[1]["type"] != "image" || parts[1]["image"] != pngURL {
		t.Fatalf("image did not survive the round trip: %v", parts)
	}
	if tools := params["tools"].([]any); len(tools) != 1 ||
		tools[0].(map[string]any)["name"] != "submit_sheet_review" {
		t.Fatalf("tool did not survive: %v", params["tools"])
	}
	tc := params["tool_choice"].(map[string]any)
	if tc["type"] != "tool" || tc["name"] != "submit_sheet_review" {
		t.Fatalf("tool_choice = %v, want the verified {type:tool,name:…}", tc)
	}

	// ...and the reply direction: NDJSON tool deltas → OpenAI tool_calls
	nd := `{"type":"tool-input-start","id":"c1","toolName":"submit_sheet_review"}` + "\n" +
		`{"type":"tool-input-delta","id":"c1","delta":"{\"sheet_number\":\"A-1"}` + "\n" +
		`{"type":"tool-input-delta","id":"c1","delta":"01\",\"findings\":[]}"}` + "\n" +
		`{"type":"finish","finishReason":"tool-calls","usage":{"inputTokens":9,"outputTokens":4,"cachedInputTokens":0}}` + "\n"
	resp, err := ParseCommandCodeResponse([]byte(nd))
	if err != nil {
		t.Fatalf("parse response: %v", err)
	}
	oai := BuildOpenAIResponse(resp)
	var out struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				ToolCalls []struct {
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(oai, &out); err != nil {
		t.Fatalf("openai response is not valid JSON: %v", err)
	}
	if len(out.Choices) == 0 || len(out.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("client did not receive a tool call: %s", oai)
	}
	got := out.Choices[0].Message.ToolCalls[0]
	if got.Function.Name != "submit_sheet_review" {
		t.Errorf("tool name = %q", got.Function.Name)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(got.Function.Arguments), &args); err != nil {
		t.Fatalf("client received invalid JSON arguments %q: %v", got.Function.Arguments, err)
	}
	if args["sheet_number"] != "A-101" {
		t.Errorf("arguments reassembled wrong: %v", args)
	}
	if out.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q", out.Choices[0].FinishReason)
	}
}

// An upstream refusal must never reach the client as an empty success. The
// gateway spells some errors as an object, and decoding that field as a string
// made the whole event unparseable — and the readers skip what they cannot
// parse, so the request looked like a clean, empty answer.
func TestAlphaObjectErrorEventSurfaces(t *testing.T) {
	nd := `{"type":"start"}` + "\n" +
		`{"type":"error","error":{"type":"cmd_zdr_no_providers","code":"CMD_ZDR_NO_PROVIDERS","message":"This model has no zero-data-retention upstream.","statusCode":400,"isRetryable":false}}` + "\n"

	if _, err := ParseCommandCodeResponse([]byte(nd)); err == nil {
		t.Fatal("buffered path returned success for an upstream error event")
	} else if !strings.Contains(err.Error(), "zero-data-retention") {
		t.Errorf("error message lost: %v", err)
	}

	ch := make(chan Delta, 8)
	go ReadCommandCodeStream(strings.NewReader(nd), ch)
	var gotErr error
	for d := range ch {
		if d.Err != nil {
			gotErr = d.Err
		}
	}
	if gotErr == nil {
		t.Fatal("streaming path swallowed an upstream error event")
	}
	if !strings.Contains(gotErr.Error(), "zero-data-retention") {
		t.Errorf("streamed error message lost: %v", gotErr)
	}

	// the bare-string spelling still works
	if _, err := ParseCommandCodeResponse([]byte(`{"type":"error","error":"plain string failure"}` + "\n")); err == nil ||
		!strings.Contains(err.Error(), "plain string failure") {
		t.Errorf("string-form error not surfaced: %v", err)
	}
}

// A tool call cut off at max_tokens arrives with its final event's "input" as
// a JSON string of the partial text. Overwriting the streamed deltas with that
// gave clients double-encoded arguments: json.Unmarshal returned a string
// where the schema promised an object.
func TestAlphaTruncatedToolCallNotDoubleEncoded(t *testing.T) {
	partial := `{"sheet":{"number":"A-101"},"findings":[{"id":"F1","note":"cut off here`
	strForm, _ := json.Marshal(partial) // the string spelling upstream sends
	nd := `{"type":"tool-input-start","id":"c1","toolName":"submit_sheet_review"}` + "\n" +
		`{"type":"tool-input-delta","id":"c1","delta":` + string(mustJSON(partial)) + "}\n" +
		`{"type":"tool-call","toolCallId":"c1","toolName":"submit_sheet_review","input":` + string(strForm) + "}\n" +
		`{"type":"finish","finishReason":"length"}` + "\n"

	r, err := ParseCommandCodeResponse([]byte(nd))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(r.ToolCalls) != 1 {
		t.Fatalf("want 1 tool call, got %d", len(r.ToolCalls))
	}
	args := r.ToolCalls[0].Args
	if strings.HasPrefix(strings.TrimSpace(args), `"`) {
		t.Fatalf("arguments are double-encoded: %q", args[:40])
	}
	if args != partial {
		t.Errorf("streamed arguments were replaced:\n got  %q\n want %q", args, partial)
	}
	// a well-formed final event still repairs fragmented deltas
	nd2 := `{"type":"tool-input-start","id":"c2","toolName":"t"}` + "\n" +
		`{"type":"tool-input-delta","id":"c2","delta":"{\"a\""}` + "\n" +
		`{"type":"tool-input-delta","id":"c2","delta":"broken"}` + "\n" +
		`{"type":"tool-call","toolCallId":"c2","toolName":"t","input":{"a":1}}` + "\n"
	r2, err := ParseCommandCodeResponse([]byte(nd2))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if r2.ToolCalls[0].Args != `{"a":1}` {
		t.Errorf("repair path broken: %q", r2.ToolCalls[0].Args)
	}
	// and valid streamed deltas are never second-guessed
	nd3 := `{"type":"tool-input-start","id":"c3","toolName":"t"}` + "\n" +
		`{"type":"tool-input-delta","id":"c3","delta":"{\"good\":true}"}` + "\n" +
		`{"type":"tool-call","toolCallId":"c3","toolName":"t","input":{"stale":1}}` + "\n"
	r3, err := ParseCommandCodeResponse([]byte(nd3))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if r3.ToolCalls[0].Args != `{"good":true}` {
		t.Errorf("valid streamed args were overwritten: %q", r3.ToolCalls[0].Args)
	}
}

func mustJSON(s string) []byte {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return b
}
