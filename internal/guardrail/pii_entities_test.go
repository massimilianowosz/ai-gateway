package guardrail

import (
	"context"
	"strings"
	"testing"
)

// piiMatrix is the entity list the portal offers, with a sample the gateway
// must recognise for each. Both PII policies read this same selection, so every
// entity is exercised against detection, redaction and filtering.
var piiMatrix = []struct {
	entity string
	text   string
	span   string // the substring that must disappear from a redacted prompt
}{
	{"EMAIL_ADDRESS", "Scrivimi a mario.rossi@example.com quando puoi", "mario.rossi@example.com"},
	{"PHONE_NUMBER", "Chiamami al +39 340 1234567 domani", "+39 340 1234567"},
	{"PERSON", "Buongiorno, mi chiamo Mario Rossi e ho una domanda", "Mario Rossi"},
	{"CREDIT_CARD", "La mia carta è 4111 1111 1111 1111 scadenza 03/29", "4111 1111 1111 1111"},
	{"CRYPTO", "Manda i fondi a 0x52908400098527886E0F7030069857D2E4169EE7 grazie", "0x52908400098527886E0F7030069857D2E4169EE7"},
	{"IBAN_CODE", "Bonifico su DE89370400440532013000 entro venerdì", "DE89370400440532013000"},
	{"IP_ADDRESS", "Il server risponde su 192.168.1.100 in LAN", "192.168.1.100"},
	{"US_SSN", "His SSN is 123-45-6789 on file", "123-45-6789"},
	{"US_BANK_NUMBER", "Wire it to account number 12345678901 today", "12345678901"},
	{"LOCATION", "Sono residente a Milano da tre anni", "Milano"},
}

// selection models a tenant that has configured entities, as opposed to one
// that never did (a nil pointer).
func selection(entities ...string) *[]string {
	chosen := append([]string(nil), entities...)
	return &chosen
}

func TestPIIScanner_DetectsWholeEntityMatrix(t *testing.T) {
	s := NewPIIScanner()
	for _, tc := range piiMatrix {
		t.Run(tc.entity, func(t *testing.T) {
			result, err := s.ScanWithEntities(context.Background(), "", tc.text, []string{tc.entity})
			if err != nil {
				t.Fatal(err)
			}
			if !result.Blocked {
				t.Fatalf("expected %s to be detected in %q", tc.entity, tc.text)
			}
			if result.Category != tc.entity {
				t.Errorf("expected category %s, got %q", tc.entity, result.Category)
			}
		})
	}
}

func TestPIIScanner_RedactsWholeEntityMatrix(t *testing.T) {
	s := NewPIIScanner()
	for _, tc := range piiMatrix {
		t.Run(tc.entity, func(t *testing.T) {
			out, entities := s.Redact(tc.text, selection(tc.entity))
			if strings.Contains(out, tc.span) {
				t.Fatalf("%s left in redacted text: %q", tc.entity, out)
			}
			if !strings.Contains(out, "["+tc.entity+"]") {
				t.Errorf("expected [%s] placeholder, got %q", tc.entity, out)
			}
			if len(entities) != 1 || entities[0] != tc.entity {
				t.Errorf("expected reported entities [%s], got %v", tc.entity, entities)
			}
		})
	}
}

func TestPIIScanner_EntitySelectionIsRespected(t *testing.T) {
	s := NewPIIScanner()
	text := "mario.rossi@example.com risponde su 192.168.1.100"

	result, err := s.ScanWithEntities(context.Background(), "", text, []string{"IP_ADDRESS"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Category != "IP_ADDRESS" {
		t.Fatalf("expected only IP_ADDRESS to be reported, got %q", result.Category)
	}

	out, entities := s.Redact(text, selection("IP_ADDRESS"))
	if !strings.Contains(out, "mario.rossi@example.com") {
		t.Errorf("unselected entity was redacted: %q", out)
	}
	if strings.Contains(out, "192.168.1.100") {
		t.Errorf("selected entity survived redaction: %q", out)
	}
	if len(entities) != 1 || entities[0] != "IP_ADDRESS" {
		t.Errorf("expected [IP_ADDRESS], got %v", entities)
	}
}

// A tenant that never opened the entity dialog keeps the coverage the scanner
// had before the list existed.
func TestPIIScanner_NoSelectionCoversTheWholeMatrix(t *testing.T) {
	s := NewPIIScanner()
	for _, tc := range piiMatrix {
		result, err := s.Scan(context.Background(), "", tc.text)
		if err != nil {
			t.Fatal(err)
		}
		if !result.Blocked {
			t.Errorf("%s not detected without a selection", tc.entity)
		}
		if out, _ := s.Redact(tc.text, nil); strings.Contains(out, tc.span) {
			t.Errorf("%s not redacted without a selection: %q", tc.entity, out)
		}
	}
}

// Unticking every box means "redact nothing", not "redact everything".
func TestPIIScanner_EmptySelectionDisablesEveryEntity(t *testing.T) {
	s := NewPIIScanner()
	for _, tc := range piiMatrix {
		result, err := s.ScanWithEntities(context.Background(), "", tc.text, []string{})
		if err != nil {
			t.Fatal(err)
		}
		if result.Blocked {
			t.Errorf("%s detected with an empty selection: %q", tc.entity, result.Category)
		}
		out, entities := s.Redact(tc.text, selection())
		if out != tc.text || len(entities) != 0 {
			t.Errorf("%s redacted with an empty selection: %q %v", tc.entity, out, entities)
		}
	}
}

// The Italian national identifiers have no checkbox in the portal, so
// narrowing — or emptying — the selection must not switch them off.
func TestPIIScanner_ItalianIdentifiersStayOn(t *testing.T) {
	s := NewPIIScanner()

	cf, err := s.ScanWithEntities(context.Background(), "", "Il mio codice fiscale è RSSMRA85M01H501Q", []string{})
	if err != nil {
		t.Fatal(err)
	}
	if cf.Category != "CODICE_FISCALE" {
		t.Errorf("expected CODICE_FISCALE, got %q", cf.Category)
	}

	out, entities := s.Redact("Fattura a P.IVA 12345678903 di ieri", selection("EMAIL_ADDRESS"))
	if out != "Fattura a P.IVA [PARTITA_IVA] di ieri" {
		t.Errorf("expected only the number to be replaced, got %q", out)
	}
	if len(entities) != 1 || entities[0] != "PARTITA_IVA" {
		t.Errorf("expected [PARTITA_IVA], got %v", entities)
	}
}

func TestPIIScanner_RedactKeepsSurroundingText(t *testing.T) {
	s := NewPIIScanner()
	out, entities := s.Redact("Ciao, mi chiamo Mario Rossi, scrivimi a m.rossi@example.com", nil)
	if out != "Ciao, mi chiamo [PERSON], scrivimi a [EMAIL_ADDRESS]" {
		t.Fatalf("unexpected redaction: %q", out)
	}
	if len(entities) != 2 {
		t.Errorf("expected two entities, got %v", entities)
	}
}

func TestPIIScanner_RedactLeavesCleanTextAlone(t *testing.T) {
	s := NewPIIScanner()
	clean := "Buongiorno, come possiamo aiutarla oggi? Mi piace il gelato."
	out, entities := s.Redact(clean, nil)
	if out != clean {
		t.Errorf("clean text was rewritten: %q", out)
	}
	if len(entities) != 0 {
		t.Errorf("expected no entities, got %v", entities)
	}
}

func TestPIIScanner_RedactSkipsInvalidCardNumbers(t *testing.T) {
	s := NewPIIScanner()
	text := "Il numero 1234 5678 9012 3456 non è una carta"
	out, entities := s.Redact(text, selection("CREDIT_CARD"))
	if out != text {
		t.Errorf("number failing Luhn was redacted: %q", out)
	}
	if len(entities) != 0 {
		t.Errorf("expected no entities, got %v", entities)
	}
}

func TestPIIScanner_IPAddressCoversIPv4AndIPv6(t *testing.T) {
	s := NewPIIScanner()
	addresses := []string{
		"192.168.1.100",
		"2001:db8::1",
		"::1",
		"fe80::",
		"2001:0db8:85a3:0000:0000:8a2e:0370:7334",
	}

	for _, address := range addresses {
		t.Run(address, func(t *testing.T) {
			text := "server " + address + " non raggiungibile"
			result, err := s.ScanWithEntities(context.Background(), "", text, []string{"IP_ADDRESS"})
			if err != nil {
				t.Fatal(err)
			}
			if !result.Blocked || result.Category != "IP_ADDRESS" {
				t.Fatalf("address %q not detected: %+v", address, result)
			}
			redacted, entities := s.Redact(text, selection("IP_ADDRESS"))
			if strings.Contains(redacted, address) || !strings.Contains(redacted, "[IP_ADDRESS]") {
				t.Fatalf("address %q not redacted: %q", address, redacted)
			}
			if len(entities) != 1 || entities[0] != "IP_ADDRESS" {
				t.Fatalf("unexpected entities for %q: %v", address, entities)
			}
		})
	}
}

// A phone number has to start where a number can start. The international "00"
// prefix used to match inside any long digit run, so selecting PHONE_NUMBER
// alone chewed holes in IBANs and crypto addresses and reported them as phones.
func TestPIIScanner_PhoneDoesNotMatchInsideOtherIdentifiers(t *testing.T) {
	s := NewPIIScanner()
	for _, text := range []string{
		"IBAN DE89370400440532013000",
		"Wallet 0x52908400098527886E0F7030069857D2E4169EE7",
		"Account number 12345678901",
		"Carta 4111 1111 1111 1111",
	} {
		if result, _ := s.ScanWithEntities(context.Background(), "", text, []string{"PHONE_NUMBER"}); result.Blocked {
			t.Errorf("phone detector fired on %q", text)
		}
		if out, entities := s.Redact(text, selection("PHONE_NUMBER")); out != text {
			t.Errorf("phone detector rewrote %q as %q (%v)", text, out, entities)
		}
	}

	// The formats it must still catch.
	for _, text := range []string{
		"Chiamami al +39 340 1234567",
		"Call 0039 340 1234567",
		"Chiamami al 340 1234567",
	} {
		if result, _ := s.ScanWithEntities(context.Background(), "", text, []string{"PHONE_NUMBER"}); !result.Blocked {
			t.Errorf("phone not detected in %q", text)
		}
	}
}
