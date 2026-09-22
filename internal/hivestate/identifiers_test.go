package hivestate

import (
	"fmt"
	"strings"
	"testing"
)

func TestExtractCodeRegistry_GoCode(t *testing.T) {
	messages := []Message{
		{Role: "assistant", Content: "Let me read /src/auth/service.go"},
		{Role: "tool", Content: `package auth

import "database/sql"

func getUserByEmail(db *sql.DB, email string) (*User, error) {
	return nil, nil
}

func (s *AuthService) ValidateToken(token string) bool {
	return true
}

type User struct {
	ID    int
	Email string
}

type AuthService interface {
	Login(email, password string) error
}
`},
	}

	reg := ExtractCodeRegistry(messages)

	if len(reg.FileEntries) == 0 {
		t.Fatal("expected file entries")
	}
	fe := reg.FileEntries[0]
	assertContainsID(t, fe.Declared, "getUserByEmail()")
	assertContainsID(t, fe.Declared, "AuthService.ValidateToken()")
	assertContainsID(t, fe.Declared, "User")
	assertContainsID(t, fe.Declared, "AuthService")
}

func TestExtractCodeRegistry_Python(t *testing.T) {
	messages := []Message{
		{Role: "assistant", Content: "Reading /src/services/user_service.py"},
		{Role: "tool", Content: `class UserService:
    def get_user_by_email(self, email: str) -> User:
        pass

    def create_user(self, data: dict) -> User:
        pass

RATE_LIMIT = 100
API_TIMEOUT = 30
`},
	}

	reg := ExtractCodeRegistry(messages)

	if len(reg.FileEntries) == 0 {
		t.Fatal("expected file entries")
	}
	fe := reg.FileEntries[0]
	assertContainsID(t, fe.Declared, "get_user_by_email()")
	assertContainsID(t, fe.Declared, "create_user()")
	assertContainsID(t, fe.Declared, "UserService")
	assertContainsID(t, reg.Loose.Constants, "RATE_LIMIT")
	assertContainsID(t, reg.Loose.Constants, "API_TIMEOUT")
}

func TestExtractCodeRegistry_JavaScript(t *testing.T) {
	messages := []Message{
		{Role: "assistant", Content: "File /src/orders/repository.ts:"},
		{Role: "tool", Content: `export class OrderRepository {
  async function processOrder(order) {
    return order;
  }
}

export function calculateTotal(items) {
  return items.reduce((sum, item) => sum + item.price, 0);
}

const MAX_ITEMS = 50;
`},
	}

	reg := ExtractCodeRegistry(messages)

	if len(reg.FileEntries) == 0 {
		t.Fatal("expected file entries")
	}
	fe := reg.FileEntries[0]
	assertContainsID(t, fe.Declared, "processOrder()")
	assertContainsID(t, fe.Declared, "calculateTotal()")
	assertContainsID(t, fe.Declared, "OrderRepository")
	assertContainsID(t, reg.Loose.Constants, "MAX_ITEMS")
}

func TestExtractCodeRegistry_Rust(t *testing.T) {
	messages := []Message{
		{Role: "assistant", Content: "Reading /src/config/manager.rs"},
		{Role: "tool", Content: `pub struct ConfigManager {
    settings: HashMap<String, String>,
}

impl ConfigManager {
    pub fn load_from_file(path: &str) -> Result<Self, Error> {
        todo!()
    }

    fn validate_config(&self) -> bool {
        true
    }
}

pub enum AppState {
    Running,
    Stopped,
}
`},
	}

	reg := ExtractCodeRegistry(messages)

	if len(reg.FileEntries) == 0 {
		t.Fatal("expected file entries")
	}
	fe := reg.FileEntries[0]
	assertContainsID(t, fe.Declared, "load_from_file()")
	assertContainsID(t, fe.Declared, "validate_config()")
	assertContainsID(t, fe.Declared, "ConfigManager")
	assertContainsID(t, fe.Declared, "AppState")
}

func TestExtractCodeRegistry_Java(t *testing.T) {
	messages := []Message{
		{Role: "assistant", Content: "File /src/payments/PaymentProcessor.java"},
		{Role: "tool", Content: `public class PaymentProcessor {
    private PaymentGateway gateway;

    public PaymentResult processPayment(Order order, CreditCard card) {
        return gateway.charge(card, order.getTotal());
    }

    private void validateCard(CreditCard card) {
        // validation logic
    }
}

public interface PaymentGateway {
    PaymentResult charge(CreditCard card, BigDecimal amount);
}
`},
	}

	reg := ExtractCodeRegistry(messages)

	if len(reg.FileEntries) == 0 {
		t.Fatal("expected file entries")
	}
	fe := reg.FileEntries[0]
	assertContainsID(t, fe.Declared, "processPayment()")
	assertContainsID(t, fe.Declared, "validateCard()")
	assertContainsID(t, fe.Declared, "PaymentProcessor")
	assertContainsID(t, fe.Declared, "PaymentGateway")
}

func TestExtractCodeRegistry_Kotlin(t *testing.T) {
	messages := []Message{
		{Role: "assistant", Content: "Reading /src/main/kotlin/UserProfile.kt"},
		{Role: "tool", Content: `data class UserProfile(val name: String, val email: String)

sealed class Result<T> {
    data class Success<T>(val data: T) : Result<T>()
}

fun fetchUserProfile(userId: String): UserProfile {
    return api.getUser(userId)
}

suspend fun loadAsync(url: String): Response {
    return client.get(url)
}
`},
	}

	reg := ExtractCodeRegistry(messages)

	if len(reg.FileEntries) == 0 {
		t.Fatal("expected file entries")
	}
	fe := reg.FileEntries[0]
	assertContainsID(t, fe.Declared, "fetchUserProfile()")
	assertContainsID(t, fe.Declared, "loadAsync()")
	assertContainsID(t, fe.Declared, "UserProfile")
	assertContainsID(t, fe.Declared, "Result")
}

func TestExtractCodeRegistry_Swift(t *testing.T) {
	messages := []Message{
		{Role: "assistant", Content: "File /src/Network/APIClient.swift"},
		{Role: "tool", Content: `protocol NetworkService {
    func fetchData(from url: URL) async throws -> Data
}

struct APIClient {
    func sendRequest(endpoint: String) -> Response {
        return Response()
    }
}

enum NetworkError {
    case timeout
    case unauthorized
}
`},
	}

	reg := ExtractCodeRegistry(messages)

	if len(reg.FileEntries) == 0 {
		t.Fatal("expected file entries")
	}
	fe := reg.FileEntries[0]
	assertContainsID(t, fe.Declared, "fetchData()")
	assertContainsID(t, fe.Declared, "sendRequest()")
	assertContainsID(t, fe.Declared, "NetworkService")
	assertContainsID(t, fe.Declared, "APIClient")
	assertContainsID(t, fe.Declared, "NetworkError")
}

func TestExtractCodeRegistry_PHP(t *testing.T) {
	messages := []Message{
		{Role: "assistant", Content: "File /app/Http/Controllers/UserController.php"},
		{Role: "tool", Content: `<?php
class UserController {
    public function index(): Response {
        return view('users.index');
    }

    private static function validateInput(array $data): bool {
        return true;
    }
}

trait Cacheable {
    public function getCacheKey(): string {
        return static::class;
    }
}

interface Repository {
    public function findById(int $id): ?Model;
}
`},
	}

	reg := ExtractCodeRegistry(messages)

	if len(reg.FileEntries) == 0 {
		t.Fatal("expected file entries")
	}
	fe := reg.FileEntries[0]
	assertContainsID(t, fe.Declared, "index()")
	assertContainsID(t, fe.Declared, "validateInput()")
	assertContainsID(t, fe.Declared, "UserController")
	assertContainsID(t, fe.Declared, "Cacheable")
	assertContainsID(t, fe.Declared, "Repository")
}

func TestExtractCodeRegistry_CppCode(t *testing.T) {
	messages := []Message{
		{Role: "assistant", Content: "Reading /src/engine/renderer.cpp"},
		{Role: "tool", Content: `namespace GameEngine {

struct Transform {
    float x, y, z;
};

class Renderer {
public:
    void renderFrame(Scene* scene) {}
    int calculateFPS() { return 60; }
};

}
`},
	}

	reg := ExtractCodeRegistry(messages)

	if len(reg.FileEntries) == 0 {
		t.Fatal("expected file entries")
	}
	fe := reg.FileEntries[0]
	assertContainsID(t, fe.Declared, "renderFrame()")
	assertContainsID(t, fe.Declared, "calculateFPS()")
	assertContainsID(t, fe.Declared, "GameEngine")
	assertContainsID(t, fe.Declared, "Transform")
	assertContainsID(t, fe.Declared, "Renderer")
}

func TestExtractCodeRegistry_MethodCallsLoose(t *testing.T) {
	messages := []Message{
		{Role: "assistant", Content: `The problem is in store.GetUserByEmail() which returns nil.
We need to call validator.CheckPermissions() before proceeding.`},
	}

	reg := ExtractCodeRegistry(messages)

	assertContainsID(t, reg.Loose.Functions, "store.GetUserByEmail")
	assertContainsID(t, reg.Loose.Functions, "validator.CheckPermissions")
}

func TestExtractCodeRegistry_MethodCallNoise(t *testing.T) {
	messages := []Message{
		{Role: "tool", Content: `
w.WriteHeader(200)
fmt.Sprintf("hello")
json.Marshal(data)
ctx.WithContext(bg)
`},
	}

	reg := ExtractCodeRegistry(messages)

	assertNotContainsID(t, reg.Loose.Functions, "WriteHeader")
	assertNotContainsID(t, reg.Loose.Functions, "Sprintf")
	assertNotContainsID(t, reg.Loose.Functions, "Marshal")
}

func TestExtractCodeRegistry_TestFunctionsFiltered(t *testing.T) {
	messages := []Message{
		{Role: "assistant", Content: "Reading /src/auth/auth.go"},
		{Role: "tool", Content: `func TestAuthenticate_Valid(t *testing.T) {}
func BenchmarkHash(b *testing.B) {}
func realFunction() {}
`},
	}

	reg := ExtractCodeRegistry(messages)

	var allDeclared []string
	for _, fe := range reg.FileEntries {
		allDeclared = append(allDeclared, fe.Declared...)
	}
	allDeclared = append(allDeclared, reg.Loose.Functions...)

	assertNotContainsID(t, allDeclared, "TestAuthenticate_Valid()")
	assertNotContainsID(t, allDeclared, "BenchmarkHash()")
	assertContainsID(t, allDeclared, "realFunction()")
}

func TestExtractCodeRegistry_TestFilesFiltered(t *testing.T) {
	messages := []Message{
		{Role: "assistant", Content: "Reading /src/auth/auth_test.go and /src/auth/auth.go"},
		{Role: "tool", Content: "func Login() {}"},
	}

	reg := ExtractCodeRegistry(messages)

	for _, fe := range reg.FileEntries {
		if strings.Contains(fe.Path, "_test.go") {
			t.Errorf("test file should be filtered: %s", fe.Path)
		}
	}
}

func TestExtractCodeRegistry_ClaudePathsFiltered(t *testing.T) {
	messages := []Message{
		{Role: "assistant", Content: "Reading /Users/user/.claude/plans/my-plan.md and /src/app/main.go"},
		{Role: "tool", Content: "func Start() {}"},
	}

	reg := ExtractCodeRegistry(messages)

	for _, fe := range reg.FileEntries {
		if strings.Contains(fe.Path, ".claude") {
			t.Errorf(".claude path should be filtered: %s", fe.Path)
		}
	}
}

func TestExtractCodeRegistry_ShortenPath(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"/Users/max/code/ubiquum-ai-gateway/internal/auth/auth.go", "/internal/auth/auth.go"},
		{"/home/dev/project/src/components/Button.tsx", "/src/components/Button.tsx"},
		{"/app/Http/Controllers/UserController.php", "/app/Http/Controllers/UserController.php"},
		{"/very/deep/nested/path/to/some/file.go", ".../to/some/file.go"},
	}

	for _, tt := range tests {
		got := shortenPath(tt.input)
		if got != tt.want {
			t.Errorf("shortenPath(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestExtractCodeRegistry_FormatOutput(t *testing.T) {
	reg := &CodeRegistry{
		FileEntries: []FileEntry{
			{
				Path:       "/internal/auth/auth.go",
				Declared:   []string{"AuthService.ValidateToken()", "getUserByEmail()", "AuthService", "User"},
				Referenced: []string{"store.GetUser()", "config.Load()"},
				Imports:    []string{"store", "config"},
			},
			{
				Path:     "/internal/store/models.go",
				Declared: []string{"CreateUser()", "Team", "APIKey"},
			},
		},
		Loose: LooseIdentifiers{
			Functions: []string{"processPayment"},
			Constants: []string{"MAX_RETRIES"},
		},
	}

	msg := reg.FormatRegistryMessage()

	if !strings.Contains(msg, "[Code Registry]") {
		t.Error("missing header")
	}
	if !strings.Contains(msg, "/internal/auth/auth.go\n") {
		t.Error("missing file path on its own line")
	}
	if !strings.Contains(msg, "Funcs[2]: AuthService.ValidateToken(), getUserByEmail()") {
		t.Errorf("missing Funcs[n] format, got:\n%s", msg)
	}
	if !strings.Contains(msg, "Types[2]: AuthService, User") {
		t.Errorf("missing Types[n] format, got:\n%s", msg)
	}
	if !strings.Contains(msg, "Refs[2]: store.GetUser(), config.Load()") {
		t.Errorf("missing Refs[n] format, got:\n%s", msg)
	}
	if !strings.Contains(msg, "Imports: store, config") {
		t.Errorf("missing imports, got:\n%s", msg)
	}
	if !strings.Contains(msg, "Referenced:\n") {
		t.Error("missing Referenced section for loose identifiers")
	}
}

func TestExtractCodeRegistry_Deduplication(t *testing.T) {
	messages := []Message{
		{Role: "assistant", Content: "File /src/app.go"},
		{Role: "tool", Content: "func processOrder(o Order) error { return nil }"},
		{Role: "assistant", Content: "File /src/app.go"},
		{Role: "tool", Content: "func processOrder(o Order) error { return nil }"},
	}

	reg := ExtractCodeRegistry(messages)

	total := 0
	for _, fe := range reg.FileEntries {
		for _, d := range fe.Declared {
			if d == "processOrder()" {
				total++
			}
		}
	}
	if total != 1 {
		t.Errorf("processOrder appeared %d times, want 1", total)
	}
}

func TestExtractCodeRegistry_BudgetTruncation(t *testing.T) {
	var messages []Message
	for i := 0; i < 50; i++ {
		var content strings.Builder
		for j := 0; j < 10; j++ {
			fmt.Fprintf(&content, "func functionWithLongName_%d_%d() {}\n", i, j)
		}
		messages = append(messages,
			Message{Role: "assistant", Content: fmt.Sprintf("File /src/pkg%d/file%d.go", i, i)},
			Message{Role: "tool", Content: content.String()},
		)
	}

	reg := ExtractCodeRegistry(messages)
	counter := NewTokenCounter()
	reg.TruncateToTokenBudget(counter)

	msg := reg.FormatRegistryMessage()
	if counter.Count(msg) > registryTokenBudget {
		t.Errorf("registry %d tokens exceeds budget %d", counter.Count(msg), registryTokenBudget)
	}
	if len(reg.FileEntries) == 0 {
		t.Error("expected some file entries to survive truncation")
	}
}

func TestExtractCodeRegistry_EmptyMessages(t *testing.T) {
	reg := ExtractCodeRegistry(nil)
	if msg := reg.FormatRegistryMessage(); msg != "" {
		t.Errorf("expected empty for nil messages, got %q", msg)
	}

	reg = ExtractCodeRegistry([]Message{})
	if msg := reg.FormatRegistryMessage(); msg != "" {
		t.Errorf("expected empty for empty messages, got %q", msg)
	}
}

// --- helpers ---

func assertContainsID(t *testing.T, slice []string, want string) {
	t.Helper()
	for _, s := range slice {
		if s == want {
			return
		}
	}
	t.Errorf("slice %v does not contain %q", slice, want)
}

func assertNotContainsID(t *testing.T, slice []string, notWant string) {
	t.Helper()
	for _, s := range slice {
		if s == notWant {
			t.Errorf("slice contains %q but should not", notWant)
			return
		}
	}
}
