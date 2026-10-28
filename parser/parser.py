import os
import sys
import time
import json
import logging
import redis
from curl_cffi import requests

# ==========================================
# НАЛАШТУВАННЯ ЛОГУВАННЯ
# ==========================================
logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s | %(levelname)s | %(message)s",
    datefmt="%Y-%m-%d %H:%M:%S"
)
logger = logging.getLogger(__name__)

HEADERS = {
    "accept": "application/json",
    "accept-language": "uk",
    "content-type": "application/json",
    "origin": "https://www.olx.ua",
    "referer": "https://www.olx.ua/",
    "user-agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
    "x-client": "DESKTOP",
}

GRAPHQL_QUERY = (
    "query ListingSearchQuery($searchParameters: [SearchParameter!] = []) {"
    "  clientCompatibleListings(searchParameters: $searchParameters) {"
    "    ... on ListingSuccess {"
    "      data {"
    "        id title status url created_time last_refresh_time description business"
    "        location { city { name } }"
    "        photos { link }"
    "        user { id uuid name created }"
    "        params {"
    "          key name"
    "          value {"
    "            ... on PriceParam { value currency label }"
    "          }"
    "        }"
    "      }"
    "    }"
    "  }"
    "}"
)

def fetch_one_olx_ad(category_id: str) -> dict | None:
    url = "https://www.olx.ua/apigateway/graphql"
    
    payload = {
        "query": GRAPHQL_QUERY,
        "variables": {
            "searchParameters": [
                {"key": "category_id", "value": category_id},
                {"key": "limit", "value": "1"},  # Беремо рівно 1 найсвіжіше
                {"key": "sort_by", "value": "created_at:desc"},
            ]
        }
    }

    try:
        # Додано timeout=15, щоб уникнути вічного зависання
        response = requests.post(
            url, 
            json=payload, 
            headers=HEADERS, 
            impersonate="chrome120",
            timeout=15
        )
        
        if response.status_code != 200:
            logger.error(f"Помилка запиту до OLX: HTTP {response.status_code}")
            return None

        raw_data = response.json()
        ads = raw_data.get("data", {}).get("clientCompatibleListings", {}).get("data", [])
        
        return ads[0] if ads else None
        
    except requests.RequestsError as e:
        logger.error(f"Мережева помилка curl_cffi: {e}")
        return None
    except Exception as e:
        logger.error(f"Неочікувана помилка: {e}")
        return None

# ==========================================
# ГОЛОВНИЙ ЦИКЛ 
# ==========================================
if __name__ == "__main__":
    redis_host = os.getenv("REDIS_HOST", "localhost")
    
    try:
        redis_client = redis.Redis(host=redis_host, port=6379, db=0, decode_responses=True)
        redis_client.ping()
        logger.info(f"Успішно підключено до Redis на {redis_host}:6379")
    except Exception as e:
        logger.critical(f"Не вдалося підключитися до Redis: {e}")
        sys.exit(1)

    # Зберігаємо ID останнього оголошення, щоб не слати дублікати
    last_ad_id = None

    while True:
        logger.info("Шукаю найновіше оголошення...")
        ad_data = fetch_one_olx_ad(category_id="85")
        
        if ad_data:
            ad_id = ad_data.get('id')
            title = ad_data.get('title', 'Без назви')
            
            if ad_id == last_ad_id:
                logger.info("Нових оголошень поки немає. Чекаємо...")
            else:
                logger.info(f"Знайдено НОВЕ оголошення! ID: {ad_id} | Назва: {title}")
                
                # Відправляємо у Redis з унікальним ключем
                redis_key = f"olx_ad:{ad_id}"
                ad_json = json.dumps(ad_data, ensure_ascii=False)
                redis_client.set(redis_key, ad_json)
                
                logger.info(f"Успішно збережено в Redis за ключем [{redis_key}].")
                last_ad_id = ad_id
        else:
            logger.warning("Оголошень не знайдено або сталася помилка запиту.")

        sleep_time = 60
        logger.info(f"Йду в sleep() на {sleep_time} секунд...\n" + "-"*40)
        time.sleep(sleep_time)