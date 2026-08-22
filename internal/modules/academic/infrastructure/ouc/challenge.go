package ouc

import (
	"bytes"
	"strings"

	"golang.org/x/net/html"
)

const (
	challengeRuleNone                 = "none"
	challengeRuleVisibleControl       = "visible_challenge_control"
	challengeRuleVisibleInstruction   = "visible_challenge_instruction"
	challengeRuleStandaloneValidation = "standalone_security_validation"
	challengeRuleUnparseable          = "unparseable_html"
	challengeRuleStructuredResponse   = "structured_response"
)

var (
	strongChallengeText = []string{"请完成验证码", "请输入验证码", "滑动验证", "设备确认"}
	securityText        = []string{"安全验证"}
)

type challengeDetection struct {
	Detected bool
	Rule     string
}

// detectInteractiveChallenge recognizes an active, user-visible challenge
// instead of matching arbitrary source text. Script templates, hidden DOM, and
// JSON payload values must never trigger the CAPTCHA branch.
func detectInteractiveChallenge(body []byte) challengeDetection {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return challengeDetection{Rule: challengeRuleNone}
	}
	trimmed = bytes.TrimPrefix(trimmed, []byte{0xef, 0xbb, 0xbf})
	if len(trimmed) == 0 {
		return challengeDetection{Rule: challengeRuleNone}
	}
	if trimmed[0] == '{' || trimmed[0] == '[' {
		return challengeDetection{Rule: challengeRuleStructuredResponse}
	}
	root, err := html.Parse(bytes.NewReader(trimmed))
	if err != nil {
		return challengeDetection{Rule: challengeRuleUnparseable}
	}
	var visibleText strings.Builder
	var visibleHeadingText strings.Builder
	hasLoginForm := false
	hasVisibleChallengeControl := false
	var visit func(*html.Node, bool, bool)
	visit = func(node *html.Node, ancestorHidden bool, ancestorHeading bool) {
		hidden := ancestorHidden || elementIsHidden(node)
		heading := ancestorHeading || elementIsHeading(node)
		if node.Type == html.TextNode && !hidden {
			visibleText.WriteString(node.Data)
			visibleText.WriteByte(' ')
			if heading {
				visibleHeadingText.WriteString(node.Data)
				visibleHeadingText.WriteByte(' ')
			}
		}
		if node.Type == html.ElementNode && node.Data == "input" {
			name := strings.ToLower(strings.TrimSpace(attributeValue(node, "name")))
			switch name {
			case "username", "password":
				hasLoginForm = true
			}
			if !hidden && isChallengeControlName(name) {
				hasVisibleChallengeControl = true
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			visit(child, hidden, heading)
		}
	}
	visit(root, false, false)
	if hasVisibleChallengeControl {
		return challengeDetection{
			Detected: true,
			Rule:     challengeRuleVisibleControl,
		}
	}
	text := visibleText.String()
	if containsText(text, strongChallengeText) {
		return challengeDetection{
			Detected: true,
			Rule:     challengeRuleVisibleInstruction,
		}
	}
	if !hasLoginForm &&
		containsText(visibleHeadingText.String(), securityText) {
		return challengeDetection{
			Detected: true,
			Rule:     challengeRuleStandaloneValidation,
		}
	}
	return challengeDetection{Rule: challengeRuleNone}
}

func elementIsHeading(node *html.Node) bool {
	if node == nil || node.Type != html.ElementNode {
		return false
	}
	switch node.Data {
	case "title", "h1", "h2":
		return true
	default:
		return false
	}
}

func elementIsHidden(node *html.Node) bool {
	if node == nil || node.Type != html.ElementNode {
		return false
	}
	switch node.Data {
	case "script", "style", "template":
		return true
	}
	if node.Data == "input" &&
		strings.EqualFold(strings.TrimSpace(attributeValue(node, "type")), "hidden") {
		return true
	}
	if _, found := htmlAttribute(node, "hidden"); found {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(attributeValue(node, "aria-hidden")), "true") {
		return true
	}
	style := strings.NewReplacer(" ", "", "\t", "", "\r", "", "\n", "").
		Replace(strings.ToLower(attributeValue(node, "style")))
	if strings.Contains(style, "display:none") ||
		strings.Contains(style, "visibility:hidden") {
		return true
	}
	for _, className := range strings.Fields(
		strings.ToLower(attributeValue(node, "class")),
	) {
		switch className {
		case "hidden", "hide", "d-none":
			return true
		}
	}
	return false
}

func attributeValue(node *html.Node, name string) string {
	value, _ := htmlAttribute(node, name)
	return value
}

func htmlAttribute(node *html.Node, name string) (string, bool) {
	for _, item := range node.Attr {
		if strings.EqualFold(item.Key, name) {
			return item.Val, true
		}
	}
	return "", false
}

func isChallengeControlName(name string) bool {
	return strings.Contains(name, "captcha") ||
		strings.Contains(name, "verifycode") ||
		strings.Contains(name, "verify_code") ||
		strings.Contains(name, "verify-code")
}

func containsText(text string, candidates []string) bool {
	text = strings.ToLower(text)
	for _, candidate := range candidates {
		if strings.Contains(text, strings.ToLower(candidate)) {
			return true
		}
	}
	return false
}
