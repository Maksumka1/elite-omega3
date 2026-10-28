package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// --- 1. СТРУКТУРИ ДЛЯ ВХІДНОГО JSON ВІД OLX (Redis) ---
type OLXAd struct {
	ID          int    `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Url         string `json:"url"`
	Params      []struct {
		Key   string `json:"key"`
		Value struct {
			Value    float64 `json:"value"`
			Currency string  `json:"currency"`
			Label    string  `json:"label"`
		} `json:"value"`
	} `json:"params"`
}

// --- 2. СТРУКТУРА ДЛЯ РЕЗУЛЬТАТУ ВІД ШІ ---
type PCBuild struct {
	CPU     string `json:"cpu"`
	GPU     string `json:"gpu"`
	RAM     string `json:"ram"`
	Storage string `json:"storage"`
	Notes   string `json:"notes"`
}

// --- 3. СТРУКТУРИ ДЛЯ GEMINI API ---
type GeminiRequest struct {
	Model          string  `json:"model"`
	Temperature    float64 `json:"temperature"`
	ResponseFormat struct {
		Type string `json:"type"`
	} `json:"response_format"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
}

type GeminiResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// Функція екстракції ціни з OLX-параметрів
func extractPrice(ad OLXAd) string {
	for _, param := range ad.Params {
		if param.Key == "price" && param.Value.Label != "" {
			return param.Value.Label
		}
	}
	return "Ціна не вказана"
}

// Головна функція парсингу одного оголошення
func parseAdWithModel(apiKey, modelName string, ad OLXAd) (*PCBuild, error) {
	url := "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"

	priceStr := extractPrice(ad)

	// Формуємо фінальний промпт із Назви, Опису та Ціни
	promptText := fmt.Sprintf("Витягни характеристики ПК.\nНазва: %s\nЦіна: %s\nОпис:\n%s", ad.Title, priceStr, ad.Description)

	reqBody := GeminiRequest{
		Model:       modelName,
		Temperature: 0.0,
	}
	reqBody.ResponseFormat.Type = "json_object"
	reqBody.Messages = []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}{
		{Role: "system", Content: "Ти екстрактор даних. Повертай виключно валідний JSON з ключами: cpu, gpu, ram, storage, notes."},
		{Role: "user", Content: promptText},
	}

	jsonBytes, _ := json.Marshal(reqBody)
	req, _ := http.NewRequest("POST", url, bytes.NewBuffer(jsonBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{Timeout: 60 * time.Second}

	var resp *http.Response
	var err error

	for attempt := 1; attempt <= 3; attempt++ {
		resp, err = client.Do(req)
		if err != nil {
			return nil, err
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			resp.Body.Close()
			time.Sleep(10 * time.Second)
			continue
		}
		break
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API помилка %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var geminiResp GeminiResponse
	if err := json.NewDecoder(resp.Body).Decode(&geminiResp); err != nil {
		return nil, err
	}

	rawJSON := geminiResp.Choices[0].Message.Content
	startIdx := strings.Index(rawJSON, "{")
	endIdx := strings.LastIndex(rawJSON, "}")
	if startIdx != -1 && endIdx != -1 && startIdx < endIdx {
		rawJSON = rawJSON[startIdx : endIdx+1]
	}

	var build PCBuild
	if err := json.Unmarshal([]byte(rawJSON), &build); err != nil {
		return nil, err
	}

	return &build, nil
}

func main() {
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		fmt.Println("Помилка: змінна GEMINI_API_KEY не встановлена.")
		return
	}

	redisHost := os.Getenv("REDIS_HOST")
	if redisHost == "" {
		redisHost = "localhost" // Для локального запуску без Docker
	}

	// Підключення до Redis
	rdb := redis.NewClient(&redis.Options{
		Addr: redisHost + ":6379",
	})
	ctx := context.Background()

	// Перевірка підключення
	if err := rdb.Ping(ctx).Err(); err != nil {
		fmt.Printf("Не вдалося підключитися до Redis: %v\n", err)
		time.Sleep(5 * time.Second) // Даємо час базі завантажитись
	}

	fmt.Println("🚀 Парсер запущено. Очікування нових оголошень у черзі 'olx_ads_queue'...")

	// Нескінченний цикл очікування нових завдань
	for {
		// BLPop чекає нескінченно (0), поки в черзі не з'явиться нове повідомлення
		result, err := rdb.BLPop(ctx, 0, "olx_ads_queue").Result()
		if err != nil {
			fmt.Printf("Помилка читання з Redis: %v\n", err)
			time.Sleep(2 * time.Second)
			continue
		}

		// result[0] - це ім'я черги, result[1] - сам отриманий JSON
		redisRawJSON := result[1]

		var olxAd OLXAd
		if err := json.Unmarshal([]byte(redisRawJSON), &olxAd); err != nil {
			fmt.Printf("Помилка парсингу JSON від Redis: %v\n", err)
			continue // Пропускаємо "бите" оголошення і чекаємо наступне
		}

		fmt.Printf("\nОтримано нове оголошення ID %d. Відправляємо в Gemini...\n", olxAd.ID)

		build, err := parseAdWithModel(apiKey, "gemini-3.5-flash-lite", olxAd)
		if err != nil {
			fmt.Printf("❌ Помилка ШІ: %v\n", err)
			continue
		}

		// Збереження у CSV
		saveToCSV(olxAd, build)
	}
}

func saveToCSV(ad OLXAd, build *PCBuild) {
	// Файл буде зберігатися в окремій папці /data (яку ми підключимо у Docker)
	filePath := "./data/results.csv"

	// Створюємо папку, якщо її немає
	os.MkdirAll("./data", os.ModePerm)

	file, err := os.OpenFile(filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Printf("❌ Помилка відкриття файлу: %v\n", err)
		return
	}
	defer file.Close()

	writer := csv.NewWriter(file)
	defer writer.Flush()

	fileStat, _ := file.Stat()
	if fileStat.Size() == 0 {
		writer.Write([]string{"OLX ID", "Посилання", "Назва", "Ціна", "CPU", "GPU", "RAM", "Storage", "Примітки"})
	}

	price := extractPrice(ad)
	err = writer.Write([]string{
		fmt.Sprintf("%d", ad.ID), ad.Url, ad.Title, price,
		build.CPU, build.GPU, build.RAM, build.Storage, build.Notes,
	})

	if err != nil {
		fmt.Printf("❌ Помилка запису в CSV: %v\n", err)
	} else {
		fmt.Println("✅ Дані успішно додано до results.csv!")
	}
}
