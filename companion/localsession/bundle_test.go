package localsession

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/personastack/personastack-api/pkg/client/apicontract"
)

func TestValidateBundleAcceptsProducerContractForBothOrigins(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.September, 26, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name, origin, endpoint string
	}{
		{name: "production", origin: "https://my.personastack.ai", endpoint: "https://mcp.personastack.ai/v1/mcp"},
		{name: "LAN", origin: "https://personastack.ericgreer.info", endpoint: "http://mcp.personastack.lan/v1/mcp"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			bundle := validBundle(now)
			bundle.MCPURL = test.endpoint
			if err := ValidateBundle(bundle, test.origin, now); err != nil {
				t.Fatalf("ValidateBundle() error = %v", err)
			}
		})
	}
}

func TestValidateBundleRejectsMalformedOrOutOfScopeBundle(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.September, 26, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		change func(*apicontract.LocalSessionResponse)
		origin string
	}{
		{name: "wrong app origin", origin: "https://attacker.example"},
		{name: "cross environment MCP", origin: "https://my.personastack.ai", change: func(bundle *apicontract.LocalSessionResponse) { bundle.MCPURL = "http://mcp.personastack.lan/v1/mcp" }},
		{name: "credential shape", change: func(bundle *apicontract.LocalSessionResponse) { bundle.BearerToken = "secret" }},
		{name: "unsupported harness", change: func(bundle *apicontract.LocalSessionResponse) { bundle.Harness = "shell" }},
		{name: "wrong expiry", change: func(bundle *apicontract.LocalSessionResponse) {
			bundle.ExpiresAt = now.Add(364 * 24 * time.Hour).Format(time.RFC3339)
		}},
		{name: "future issue", change: func(bundle *apicontract.LocalSessionResponse) {
			bundle.IssuedAt = now.Add(6 * time.Minute).Format(time.RFC3339Nano)
		}},
		{name: "digest mismatch", change: func(bundle *apicontract.LocalSessionResponse) {
			bundle.Skills[0].Digest = "sha256:" + stringsOf("0", 64)
		}},
		{name: "unsafe skill path", change: func(bundle *apicontract.LocalSessionResponse) { bundle.Skills[0].Files[0].RelativePath = "../SKILL.md" }},
		{name: "file ancestor collision", change: func(bundle *apicontract.LocalSessionResponse) {
			bundle.Skills[0].Files = append(bundle.Skills[0].Files, apicontract.LocalSessionSkillFile{RelativePath: "SKILL.md/child", Content: "x"})
		}},
		{name: "duplicate skill identity", change: func(bundle *apicontract.LocalSessionResponse) {
			bundle.Skills = append(bundle.Skills, bundle.Skills[0])
		}},
		{name: "unsupported MCP endpoint path", change: func(bundle *apicontract.LocalSessionResponse) { bundle.MCPURL += "?other=1" }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			bundle := validBundle(now)
			if test.change != nil {
				test.change(&bundle)
			}
			origin := test.origin
			if origin == "" {
				origin = "https://my.personastack.ai"
			}
			if err := ValidateBundle(bundle, origin, now); !errors.Is(err, ErrInvalidBundle) {
				t.Fatalf("ValidateBundle() error = %v", err)
			}
		})
	}
}

func TestValidateBundleEnforcesAggregateSkillLimits(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.September, 26, 12, 0, 0, 0, time.UTC)
	bundle := validBundle(now)
	bundle.Skills = make([]apicontract.LocalSessionSkill, maxSessionSkills+1)
	for index := range bundle.Skills {
		bundle.Skills[index] = makeSkill(fmt.Sprintf("skill-%d", index), "SKILL.md", "contents")
	}
	if err := ValidateBundle(bundle, "https://my.personastack.ai", now); !errors.Is(err, ErrInvalidBundle) {
		t.Fatalf("ValidateBundle() excessive skills error = %v", err)
	}
}

func validBundle(now time.Time) apicontract.LocalSessionResponse {
	issued := now.Format(time.RFC3339Nano)
	return apicontract.LocalSessionResponse{
		PersonaID: "persona_one", PersonaName: "Assistant", WorkspaceID: "ws_0123456789abcdef0123456789abcdef",
		Harness: apicontract.LocalSessionHarnessCodex, IssuedAt: issued, ExpiresAt: now.Add(sessionLifetime).Format(time.RFC3339Nano),
		MCPURL: "https://mcp.personastack.ai/v1/mcp", BearerToken: stringsOf("a", 64), PersonaPrompt: "Use current persona context.",
		Skills: []apicontract.LocalSessionSkill{makeSkill("skill-1", "SKILL.md", "---\nname: example\ndescription: Example skill.\n---\n")},
	}
}

func makeSkill(id, file, content string) apicontract.LocalSessionSkill {
	skill := apicontract.LocalSessionSkill{SkillID: id, Slug: "example", Files: []apicontract.LocalSessionSkillFile{{RelativePath: file, Content: content}}}
	skill.Digest = skillFileDigest(skill.Files)
	return skill
}

func stringsOf(character string, length int) string {
	result := ""
	for range length {
		result += character
	}
	return result
}
