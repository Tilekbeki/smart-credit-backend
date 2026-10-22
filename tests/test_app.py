import json
from pathlib import Path
import tempfile
from threading import Thread
import unittest
from urllib.error import HTTPError
from urllib.request import Request, urlopen
from http.server import ThreadingHTTPServer

from backend.app import (
    APPLICATION_STATUSES, ApiError, ApplicationService, calculate_offer, make_handler,
)


VALID_APPLICATION = {
    "profile_id": "shop",
    "goal_id": "stock",
    "goal_description": "",
    "product_id": "turnover",
    "amount": 1_500_000,
    "term_months": 12,
    "contact_name": "Алия Соколова",
    "contact_role": "Владелец бизнеса",
}


def approved_score(payload):
    return {
        "decision": "approved", "score": 82,
        "approved_amount": payload["requested_amount"],
        "reasons": ["Достаточный денежный поток по учебной модели"],
        "factors": {"available_cash_flow": 810000},
    }


class ApplicationServiceTests(unittest.TestCase):
    def setUp(self):
        self.temp_dir = tempfile.TemporaryDirectory()
        self.db_path = str(Path(self.temp_dir.name) / "applications.sqlite3")
        self.scoring_requests = []

        def score(payload):
            self.scoring_requests.append(payload)
            return approved_score(payload)

        self.service = ApplicationService(self.db_path, "http://unused", score)

    def tearDown(self):
        self.temp_dir.cleanup()

    def test_full_self_service_flow_persists_and_disbursement_is_idempotent(self):
        application = self.service.create(VALID_APPLICATION)
        self.assertEqual(application["status"], "submitted")
        self.assertTrue(application["simulation"])
        application_id = application["id"]
        self.assertEqual(self.service.get(application_id)["status"], "submitted")

        approved = self.service.evaluate(application_id)
        self.assertEqual(approved["status"], "approved")
        self.assertEqual(approved["decision"], "approved")
        self.assertEqual(approved["approved_amount"], 1_500_000)
        self.assertEqual(approved["offer"]["annual_rate_percent"], 20.0)
        self.assertEqual(len(self.scoring_requests), 1)
        self.assertEqual(self.scoring_requests[0]["monthly_revenue"], 2_400_000)
        self.assertEqual(self.scoring_requests[0]["requested_amount"], 1_500_000)
        self.assertEqual(self.service.evaluate(application_id)["status"], "approved")
        self.assertEqual(len(self.scoring_requests), 1)

        signed = self.service.accept(application_id, {"accepted": True})
        self.assertEqual(signed["status"], "signed")
        self.assertEqual(signed["acceptance"]["method"], "prototype_click_confirmation")
        self.assertEqual(self.service.accept(application_id, {"accepted": True})["status"], "signed")

        funded = self.service.disburse(application_id)
        self.assertEqual(funded["status"], "funded")
        self.assertEqual(funded["receipt"]["amount"], 1_500_000)
        self.assertIn("Учебный счёт", funded["receipt"]["account_masked"])
        same_receipt = self.service.disburse(application_id)["receipt"]
        self.assertEqual(same_receipt, funded["receipt"])
        self.assertEqual(
            [event["status"] for event in funded["events"]],
            ["submitted", "processing", "approved", "signed", "funded"],
        )

        restarted = ApplicationService(self.db_path, "http://unused", lambda _: self.fail("No score after restart"))
        self.assertEqual(restarted.get(application_id)["receipt"], funded["receipt"])
        self.assertEqual(restarted.list()["total"], 1)

    def test_invalid_transitions_and_validation(self):
        application = self.service.create(VALID_APPLICATION)
        application_id = application["id"]
        with self.assertRaises(ApiError) as context:
            self.service.accept(application_id, {"accepted": True})
        self.assertEqual(context.exception.status, 409)
        with self.assertRaises(ApiError) as context:
            self.service.disburse(application_id)
        self.assertEqual(context.exception.status, 409)
        with self.assertRaises(ApiError) as context:
            self.service.accept(application_id, {"accepted": False})
        self.assertEqual(context.exception.error, "acceptance_required")
        with self.assertRaises(ApiError) as context:
            self.service.create({**VALID_APPLICATION, "amount": -1})
        self.assertEqual(context.exception.status, 400)
        with self.assertRaises(ApiError) as context:
            self.service.create({**VALID_APPLICATION, "profile_id": "unknown"})
        self.assertEqual(context.exception.error, "invalid_profile")
        with self.assertRaises(ApiError) as context:
            self.service.get("SC-DOES-NOT-EXIST")
        self.assertEqual(context.exception.status, 404)

    def test_rejected_application_cannot_be_signed(self):
        self.service.score_client = lambda _: {
            "decision": "rejected", "score": 18, "approved_amount": 0,
            "reasons": ["Денежного потока недостаточно"], "factors": {},
        }
        application_id = self.service.create(VALID_APPLICATION)["id"]
        rejected = self.service.evaluate(application_id)
        self.assertEqual(rejected["status"], "rejected")
        self.assertIsNone(rejected["offer"])
        with self.assertRaises(ApiError) as context:
            self.service.accept(application_id, {"accepted": True})
        self.assertEqual(context.exception.status, 409)

    def test_counter_offer_uses_reduced_amount(self):
        self.service.score_client = lambda payload: {
            "decision": "counter_offer", "score": 62,
            "approved_amount": payload["requested_amount"] // 2,
            "reasons": ["Доступная сумма ниже запрошенной"], "factors": {},
        }
        application_id = self.service.create(VALID_APPLICATION)["id"]
        counter = self.service.evaluate(application_id)
        self.assertEqual(counter["status"], "counter_offer")
        self.assertEqual(counter["offer"]["amount"], 750_000)
        self.service.accept(application_id, {"accepted": True})
        self.assertEqual(self.service.disburse(application_id)["receipt"]["amount"], 750_000)

    def test_scoring_failure_is_visible_and_can_be_retried(self):
        attempts = 0

        def flaky_score(payload):
            nonlocal attempts
            attempts += 1
            if attempts == 1:
                raise ApiError(502, "scoring_unavailable", "Временная ошибка")
            return approved_score(payload)

        self.service.score_client = flaky_score
        application_id = self.service.create(VALID_APPLICATION)["id"]
        with self.assertRaises(ApiError) as context:
            self.service.evaluate(application_id)
        self.assertEqual(context.exception.status, 502)
        after_failure = self.service.get(application_id)
        self.assertEqual(after_failure["status"], "processing")
        self.assertEqual(after_failure["evaluation_error"], "Временная ошибка")
        result = self.service.evaluate(application_id)
        self.assertEqual(result["status"], "approved")
        self.assertIsNone(result["evaluation_error"])
        self.assertEqual([event["status"] for event in result["events"]].count("processing"), 1)

    def test_offer_is_illustrative_and_amortized(self):
        offer = calculate_offer(1_500_000, 12)
        self.assertTrue(offer["illustrative"])
        self.assertGreater(offer["monthly_payment"], 125_000)
        self.assertEqual(offer["total_payment"], 12 * offer["monthly_payment"])
        self.assertEqual(offer["overpayment"], offer["total_payment"] - offer["amount"])
        self.assertIn("funded", APPLICATION_STATUSES)


class HttpApiTests(unittest.TestCase):
    def setUp(self):
        self.temp_dir = tempfile.TemporaryDirectory()
        service = ApplicationService(
            str(Path(self.temp_dir.name) / "http.sqlite3"), "http://unused", approved_score
        )
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), make_handler(service))
        self.thread = Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.base = f"http://127.0.0.1:{self.server.server_port}"

    def tearDown(self):
        self.server.shutdown()
        self.thread.join(timeout=2)
        self.server.server_close()
        self.temp_dir.cleanup()

    def request(self, method, path, payload=None, origin=None):
        headers = {}
        if origin:
            headers["Origin"] = origin
        data = None
        if payload is not None:
            data = json.dumps(payload, ensure_ascii=False).encode()
            headers["Content-Type"] = "application/json"
        request = Request(self.base + path, data=data, headers=headers, method=method)
        try:
            with urlopen(request) as response:
                return response.status, json.load(response), response.headers
        except HTTPError as error:
            with error:
                return error.code, json.load(error), error.headers

    def test_http_route_and_error_contract(self):
        status, health, _ = self.request("GET", "/health")
        self.assertEqual((status, health["status"]), (200, "ok"))
        status, created, headers = self.request(
            "POST", "/api/v1/applications", VALID_APPLICATION, "http://localhost:5173"
        )
        self.assertEqual(status, 201)
        self.assertEqual(headers["Access-Control-Allow-Origin"], "http://localhost:5173")
        application_id = created["id"]
        status, evaluated, _ = self.request("POST", f"/api/v1/applications/{application_id}/evaluate")
        self.assertEqual((status, evaluated["status"]), (200, "approved"))
        status, error, _ = self.request("POST", f"/api/v1/applications/{application_id}/accept", {"accepted": False})
        self.assertEqual((status, error["error"]), (400, "acceptance_required"))
        status, signed, _ = self.request("POST", f"/api/v1/applications/{application_id}/accept", {"accepted": True})
        self.assertEqual((status, signed["status"]), (200, "signed"))
        status, funded, _ = self.request("POST", f"/api/v1/applications/{application_id}/disburse")
        self.assertEqual((status, funded["status"]), (200, "funded"))
        status, saved, _ = self.request("GET", f"/api/v1/applications/{application_id}")
        self.assertEqual((status, saved["receipt"]), (200, funded["receipt"]))

    def test_invalid_json_and_unknown_route(self):
        status, error, _ = self.request("POST", "/api/v1/applications", {"profile_id": "shop"})
        self.assertEqual(status, 400)
        self.assertEqual(error["error"], "invalid_request")
        status, error, _ = self.request("GET", "/missing")
        self.assertEqual((status, error["error"]), (404, "not_found"))


if __name__ == "__main__":
    unittest.main()
