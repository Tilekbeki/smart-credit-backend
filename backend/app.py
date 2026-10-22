"""Self-service credit journey over synthetic data for a hackathon prototype.

No endpoint calls a bank, signs a legal contract, or transfers real money.
"""

from __future__ import annotations

from contextlib import contextmanager
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import re
import sqlite3
from threading import RLock
from urllib.error import HTTPError, URLError
from urllib.parse import urlparse
from urllib.request import Request, urlopen
from uuid import uuid4


APPLICATION_STATUSES = (
    "submitted", "processing", "approved", "counter_offer", "rejected",
    "signed", "funded",
)
DECISIONS = {"approved", "counter_offer", "rejected"}
PRODUCT_IDS = {"turnover", "investment"}
GOAL_IDS = {"stock", "expenses", "seasonal", "equipment", "upgrade", "other"}
ILLUSTRATIVE_ANNUAL_RATE_PERCENT = 20.0
ORIGIN_PATTERN = re.compile(r"^http://(?:localhost|127\.0\.0\.1)(?::\d{1,5})?$")
APPLICATION_PATH = re.compile(r"^/api/v1/applications/([A-Za-z0-9-]+)(?:/(evaluate|accept|disburse))?$")

PROFILES = {
    "shop": {
        "name": "ИП «Лавка у дома»",
        "monthly_revenue": 2_400_000, "monthly_expenses": 1_500_000,
        "existing_debt": 500_000, "monthly_debt_payments": 90_000,
        "overdue_count": 0, "account_masked": "Учебный счёт •• 4301",
    },
    "workshop": {
        "name": "ООО «Мастерская Север»",
        "monthly_revenue": 4_200_000, "monthly_expenses": 2_800_000,
        "existing_debt": 1_200_000, "monthly_debt_payments": 180_000,
        "overdue_count": 0, "account_masked": "Учебный счёт •• 5802",
    },
    "cafe": {
        "name": "ООО «Точка вкуса»",
        "monthly_revenue": 1_700_000, "monthly_expenses": 1_320_000,
        "existing_debt": 700_000, "monthly_debt_payments": 130_000,
        "overdue_count": 1, "account_masked": "Учебный счёт •• 2603",
    },
    "logistics": {
        "name": "ООО «Маршрут плюс»",
        "monthly_revenue": 3_100_000, "monthly_expenses": 2_250_000,
        "existing_debt": 1_800_000, "monthly_debt_payments": 220_000,
        "overdue_count": 0, "account_masked": "Учебный счёт •• 7404",
    },
}


def utc_now() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="seconds").replace("+00:00", "Z")


class ApiError(Exception):
    def __init__(self, status: int, error: str, message: str):
        super().__init__(message)
        self.status = status
        self.error = error
        self.message = message


def _required_string(payload: dict, name: str, max_length: int = 160) -> str:
    value = payload.get(name)
    if not isinstance(value, str) or not value.strip() or len(value.strip()) > max_length:
        raise ApiError(400, "invalid_request", f"Поле {name} должно быть непустой строкой до {max_length} символов.")
    return " ".join(value.split())


def _positive_integer(payload: dict, name: str, maximum: int) -> int:
    value = payload.get(name)
    if isinstance(value, bool) or not isinstance(value, int) or not 0 < value <= maximum:
        raise ApiError(400, "invalid_request", f"Поле {name} должно быть целым числом от 1 до {maximum}.")
    return value


def validate_application(payload: object) -> dict:
    if not isinstance(payload, dict):
        raise ApiError(400, "invalid_request", "Передайте объект заявки в JSON.")
    profile_id = _required_string(payload, "profile_id", 32)
    if profile_id not in PROFILES:
        raise ApiError(400, "invalid_profile", "Выберите доступный учебный профиль компании.")
    goal_id = _required_string(payload, "goal_id", 32)
    if goal_id not in GOAL_IDS:
        raise ApiError(400, "invalid_goal", "Выберите доступную цель финансирования.")
    goal_description = payload.get("goal_description", "")
    if not isinstance(goal_description, str) or len(goal_description.strip()) > 300:
        raise ApiError(400, "invalid_request", "Описание цели должно быть строкой до 300 символов.")
    goal_description = " ".join(goal_description.split())
    if goal_id == "other" and len(goal_description) < 4:
        raise ApiError(400, "invalid_goal", "Опишите свою задачу хотя бы четырьмя символами.")
    product_id = _required_string(payload, "product_id", 32)
    if product_id not in PRODUCT_IDS:
        raise ApiError(400, "invalid_product", "Выберите доступный кредитный продукт.")
    amount = _positive_integer(payload, "amount", 100_000_000)
    term_months = _positive_integer(payload, "term_months", 120)
    contact_name = _required_string(payload, "contact_name", 120)
    if len(contact_name.split()) < 2 or any(char.isdigit() for char in contact_name):
        raise ApiError(400, "invalid_contact", "Укажите фамилию и имя контактного лица без цифр.")
    contact_role = _required_string(payload, "contact_role", 120)
    return {
        "profile_id": profile_id, "goal_id": goal_id,
        "goal_description": goal_description, "product_id": product_id,
        "amount": amount, "term_months": term_months,
        "contact_name": contact_name, "contact_role": contact_role,
    }


def calculate_offer(amount: int, term_months: int) -> dict:
    rate = ILLUSTRATIVE_ANNUAL_RATE_PERCENT / 100 / 12
    monthly_payment = round(amount * rate / (1 - (1 + rate) ** -term_months))
    total_payment = monthly_payment * term_months
    return {
        "amount": amount, "term_months": term_months,
        "annual_rate_percent": ILLUSTRATIVE_ANNUAL_RATE_PERCENT,
        "monthly_payment": monthly_payment, "total_payment": total_payment,
        "overpayment": total_payment - amount, "illustrative": True,
    }


def request_score(scoring_url: str, payload: dict) -> dict:
    request = Request(
        f"{scoring_url.rstrip('/')}/v1/score",
        data=json.dumps(payload, ensure_ascii=False).encode("utf-8"),
        headers={"Content-Type": "application/json; charset=utf-8"},
        method="POST",
    )
    try:
        with urlopen(request, timeout=6) as response:
            data = json.load(response)
    except (HTTPError, URLError, TimeoutError, OSError, ValueError) as exc:
        raise ApiError(502, "scoring_unavailable", "Автоматическая проверка временно недоступна. Повторите попытку.") from exc
    if not isinstance(data, dict) or data.get("decision") not in DECISIONS:
        raise ApiError(502, "scoring_invalid_response", "Сервис проверки вернул некорректный результат.")
    return data


class ApplicationService:
    def __init__(self, db_path: str, scoring_url: str, score_client=None):
        self.db_path = db_path
        self.scoring_url = scoring_url
        self.score_client = score_client or (lambda payload: request_score(self.scoring_url, payload))
        self.lock = RLock()
        Path(db_path).parent.mkdir(parents=True, exist_ok=True)
        with self._connect() as connection:
            connection.execute(
                "CREATE TABLE IF NOT EXISTS applications ("
                "id TEXT PRIMARY KEY, payload TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL)"
            )

    @contextmanager
    def _connect(self):
        connection = sqlite3.connect(self.db_path, timeout=10)
        connection.row_factory = sqlite3.Row
        try:
            yield connection
            connection.commit()
        except Exception:
            connection.rollback()
            raise
        finally:
            connection.close()

    def _save(self, application: dict) -> None:
        application["updated_at"] = utc_now()
        with self._connect() as connection:
            connection.execute(
                "INSERT INTO applications (id, payload, created_at, updated_at) VALUES (?, ?, ?, ?) "
                "ON CONFLICT(id) DO UPDATE SET payload=excluded.payload, updated_at=excluded.updated_at",
                (application["id"], json.dumps(application, ensure_ascii=False),
                 application["created_at"], application["updated_at"]),
            )

    @staticmethod
    def _event(application: dict, status: str, title: str) -> None:
        application["status"] = status
        application["events"].append({"status": status, "at": utc_now(), "title": title})

    def create(self, payload: object) -> dict:
        fields = validate_application(payload)
        now = utc_now()
        application = {
            "id": f"SC-{uuid4().hex[:12].upper()}",
            "status": "submitted", "created_at": now, "updated_at": now,
            "events": [{"status": "submitted", "at": now, "title": "Заявка отправлена"}],
            "decision": None, "score": None, "approved_amount": None,
            "reasons": [], "factors": {}, "offer": None, "acceptance": None,
            "receipt": None, "evaluation_error": None, "simulation": True,
            **fields,
        }
        with self.lock:
            self._save(application)
        return application

    def get(self, application_id: str) -> dict:
        with self._connect() as connection:
            row = connection.execute("SELECT payload FROM applications WHERE id=?", (application_id,)).fetchone()
        if row is None:
            raise ApiError(404, "application_not_found", "Заявка не найдена.")
        return json.loads(row["payload"])

    def list(self) -> dict:
        with self._connect() as connection:
            rows = connection.execute("SELECT payload FROM applications ORDER BY created_at DESC, id DESC").fetchall()
        items = [json.loads(row["payload"]) for row in rows]
        return {"items": items, "total": len(items)}

    def evaluate(self, application_id: str) -> dict:
        with self.lock:
            application = self.get(application_id)
            if application["status"] in {"approved", "counter_offer", "rejected", "signed", "funded"}:
                return application
            if application["status"] not in {"submitted", "processing"}:
                raise ApiError(409, "invalid_status", "Заявку нельзя проверить на этом этапе.")
            if application["status"] != "processing":
                self._event(application, "processing", "Идёт автоматическая проверка")
            application["evaluation_error"] = None
            self._save(application)
            profile = PROFILES[application["profile_id"]]
            scoring_request = {
                "application_id": application_id,
                "monthly_revenue": profile["monthly_revenue"],
                "monthly_expenses": profile["monthly_expenses"],
                "existing_debt": profile["existing_debt"],
                "monthly_debt_payments": profile["monthly_debt_payments"],
                "overdue_count": profile["overdue_count"],
                "requested_amount": application["amount"],
                "term_months": application["term_months"],
            }
            try:
                scoring = self.score_client(scoring_request)
            except ApiError as exc:
                application["evaluation_error"] = exc.message
                self._save(application)
                raise
            except Exception as exc:
                application["evaluation_error"] = "Автоматическая проверка временно недоступна. Повторите попытку."
                self._save(application)
                raise ApiError(502, "scoring_unavailable", application["evaluation_error"]) from exc
            if not isinstance(scoring, dict) or scoring.get("decision") not in DECISIONS:
                application["evaluation_error"] = "Сервис проверки вернул некорректный результат."
                self._save(application)
                raise ApiError(502, "scoring_invalid_response", application["evaluation_error"])
            decision = scoring["decision"]
            score = scoring.get("score")
            approved_amount = scoring.get("approved_amount")
            reasons = scoring.get("reasons")
            factors = scoring.get("factors")
            invalid = (
                isinstance(score, bool) or not isinstance(score, int) or not 0 <= score <= 100
                or isinstance(approved_amount, bool) or not isinstance(approved_amount, int)
                or not 0 <= approved_amount <= application["amount"]
                or not isinstance(reasons, list) or not all(isinstance(reason, str) for reason in reasons)
                or not isinstance(factors, dict)
                or (decision == "approved" and approved_amount != application["amount"])
                or (decision == "counter_offer" and not 0 < approved_amount < application["amount"])
                or (decision == "rejected" and approved_amount != 0)
            )
            if invalid:
                application["evaluation_error"] = "Сервис проверки вернул некорректный результат."
                self._save(application)
                raise ApiError(502, "scoring_invalid_response", application["evaluation_error"])
            application["decision"] = decision
            application["score"] = score
            application["approved_amount"] = approved_amount
            application["reasons"] = reasons
            application["factors"] = factors
            application["offer"] = calculate_offer(approved_amount, application["term_months"]) if approved_amount else None
            title = {
                "approved": "Учебная заявка одобрена автоматически",
                "counter_offer": "Доступен вариант на меньшую сумму",
                "rejected": "По учебным правилам заявка не одобрена",
            }[decision]
            self._event(application, decision, title)
            self._save(application)
            return application

    def accept(self, application_id: str, payload: object) -> dict:
        if not isinstance(payload, dict) or payload.get("accepted") is not True:
            raise ApiError(400, "acceptance_required", "Подтвердите учебные условия перед продолжением.")
        with self.lock:
            application = self.get(application_id)
            if application["status"] in {"signed", "funded"}:
                return application
            if application["status"] not in {"approved", "counter_offer"}:
                raise ApiError(409, "invalid_status", "Принять условия можно после одобрения заявки.")
            application["acceptance"] = {
                "accepted_at": utc_now(), "method": "prototype_click_confirmation",
                "simulation": True,
            }
            self._event(application, "signed", "Учебные условия подтверждены")
            self._save(application)
            return application

    def disburse(self, application_id: str) -> dict:
        with self.lock:
            application = self.get(application_id)
            if application["status"] == "funded":
                return application
            if application["status"] != "signed":
                raise ApiError(409, "invalid_status", "Зачисление доступно после подтверждения условий.")
            application["receipt"] = {
                "transaction_id": f"SIM-{uuid4().hex[:12].upper()}",
                "credited_at": utc_now(),
                "account_masked": PROFILES[application["profile_id"]]["account_masked"],
                "amount": application["offer"]["amount"],
                "simulation": True,
            }
            self._event(application, "funded", "Средства зачислены на учебный счёт")
            self._save(application)
            return application


def make_handler(service: ApplicationService):
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, _format, *args):
            # Requests include synthetic contacts; avoid printing paths or bodies.
            return

        def _send_json(self, status: int, payload: dict) -> None:
            body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
            self.send_response(status)
            self.send_header("Content-Type", "application/json; charset=utf-8")
            self.send_header("Content-Length", str(len(body)))
            self.send_header("Cache-Control", "no-store")
            origin = self.headers.get("Origin", "")
            if ORIGIN_PATTERN.fullmatch(origin):
                self.send_header("Access-Control-Allow-Origin", origin)
                self.send_header("Vary", "Origin")
            self.end_headers()
            self.wfile.write(body)

        def _read_json(self) -> object:
            try:
                length = int(self.headers.get("Content-Length", "0"))
            except ValueError as exc:
                raise ApiError(400, "invalid_json", "Укажите корректную длину JSON.") from exc
            if length <= 0 or length > 65536:
                raise ApiError(400, "invalid_json", "Размер JSON должен быть от 1 до 65536 байт.")
            try:
                return json.loads(self.rfile.read(length))
            except (UnicodeDecodeError, ValueError) as exc:
                raise ApiError(400, "invalid_json", "Не удалось прочитать JSON.") from exc

        def _handle(self):
            path = urlparse(self.path).path.rstrip("/") or "/"
            if self.command == "GET" and path == "/health":
                return 200, {"status": "ok", "service": "backend"}
            if self.command == "GET" and path == "/api/v1/profiles":
                return 200, {"items": [{"id": key, "name": value["name"]} for key, value in PROFILES.items()]}
            if path == "/api/v1/applications":
                if self.command == "GET":
                    return 200, service.list()
                if self.command == "POST":
                    return 201, service.create(self._read_json())
            match = APPLICATION_PATH.fullmatch(path)
            if match:
                application_id, action = match.groups()
                if self.command == "GET" and action is None:
                    return 200, service.get(application_id)
                if self.command == "POST" and action == "evaluate":
                    return 200, service.evaluate(application_id)
                if self.command == "POST" and action == "accept":
                    return 200, service.accept(application_id, self._read_json())
                if self.command == "POST" and action == "disburse":
                    return 200, service.disburse(application_id)
            raise ApiError(404, "not_found", "Маршрут не найден.")

        def do_GET(self):
            try:
                status, payload = self._handle()
            except ApiError as exc:
                status, payload = exc.status, {"error": exc.error, "message": exc.message}
            self._send_json(status, payload)

        def do_POST(self):
            try:
                status, payload = self._handle()
            except ApiError as exc:
                status, payload = exc.status, {"error": exc.error, "message": exc.message}
            self._send_json(status, payload)

        def do_OPTIONS(self):
            origin = self.headers.get("Origin", "")
            if not ORIGIN_PATTERN.fullmatch(origin):
                self._send_json(403, {"error": "origin_forbidden", "message": "Источник запроса не разрешён."})
                return
            self.send_response(204)
            self.send_header("Access-Control-Allow-Origin", origin)
            self.send_header("Vary", "Origin")
            self.send_header("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
            self.send_header("Access-Control-Allow-Headers", "Content-Type")
            self.send_header("Content-Length", "0")
            self.end_headers()

    return Handler


def main() -> None:
    host = os.environ.get("HOST", "127.0.0.1")
    port = int(os.environ.get("PORT", "8000"))
    db_path = os.environ.get("DB_PATH", "data/applications.sqlite3")
    scoring_url = os.environ.get("SCORING_URL", "http://127.0.0.1:8001")
    service = ApplicationService(db_path, scoring_url)
    server = ThreadingHTTPServer((host, port), make_handler(service))
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()


if __name__ == "__main__":
    main()
