import unittest
from decimal import Decimal

from monthly_cost import CATEGORIES, InputError, PRODUCTS, calculate


class MonthlyCostTest(unittest.TestCase):
    def fixtures(self):
        costs = []
        for product in PRODUCTS:
            for category in CATEGORIES:
                costs.append({
                    "month": "2026-10", "cost_id": f"{product}-{category}",
                    "product": product, "category": category,
                    "amount": "100" if category == "server" else "0",
                    "currency": "CNY", "fx_to_cny": "1", "fx_ref": "",
                    "basis": "actual" if category == "server" else "confirmed_zero",
                    "source_ref": "invoice-or-zero-confirmation",
                })
        costs.append({
            "month": "2026-10", "cost_id": "shared-storage", "product": "shared",
            "category": "storage", "amount": "50", "currency": "CNY",
            "fx_to_cny": "1", "fx_ref": "", "basis": "actual", "source_ref": "invoice",
        })
        allocations = [
            {"month": "2026-10", "cost_id": "shared-storage", "product": "customer_tools",
             "weight": "0.4", "driver": "GB-month", "source_ref": "monitoring"},
            {"month": "2026-10", "cost_id": "shared-storage", "product": "ai_workspace",
             "weight": "0.6", "driver": "GB-month", "source_ref": "monitoring"},
        ]
        parameters = [
            {"month": "2026-10", "product": product, "billable_units": "2",
             "unit_name": "seat-month", "onboarding_amort_cny": "0",
             "risk_reserve_pct": "0.1", "payment_fee_pct": "0.02",
             "min_margin_pct": "0.2", "approval_status": "approved", "source_ref": "finance-approval"}
            for product in PRODUCTS
        ]
        return costs, allocations, parameters

    def test_full_allocation_and_floor(self):
        result = calculate("2026-10", *self.fixtures())
        self.assertEqual(result["status"], "actual_inputs")
        self.assertEqual(Decimal(result["products"]["customer_tools"]["monthly_cost_cny"]), Decimal("120"))
        self.assertEqual(result["products"]["customer_tools"]["quote_floor_ex_tax_cny"], "169.24")
        self.assertEqual(Decimal(result["products"]["ai_workspace"]["monthly_cost_cny"]), Decimal("130"))

    def test_unallocated_shared_cost_fails(self):
        costs, allocations, parameters = self.fixtures()
        allocations[1]["weight"] = "0.5"
        with self.assertRaisesRegex(InputError, "expected 1"):
            calculate("2026-10", costs, allocations, parameters)

    def test_estimate_is_provisional(self):
        costs, allocations, parameters = self.fixtures()
        costs[0]["basis"] = "estimate"
        self.assertEqual(calculate("2026-10", costs, allocations, parameters)["status"], "provisional")

    def test_missing_cost_category_fails(self):
        costs, allocations, parameters = self.fixtures()
        costs = [c for c in costs if c["cost_id"] != "customer_tools-model"]
        with self.assertRaisesRegex(InputError, "missing category evidence"):
            calculate("2026-10", costs, allocations, parameters)


if __name__ == "__main__":
    unittest.main()
