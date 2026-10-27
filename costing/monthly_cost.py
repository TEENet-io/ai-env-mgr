#!/usr/bin/env python3
"""Reproduce monthly product costs and internal ex-tax price floors from CSVs."""

import argparse
import csv
import json
import re
from collections import defaultdict
from decimal import Decimal, InvalidOperation, ROUND_CEILING
from pathlib import Path

PRODUCTS = ("customer_tools", "ai_workspace")
CATEGORIES = ("model", "server", "gateway", "storage", "operations")
CENT = Decimal("0.01")


class InputError(ValueError):
    pass


def read_rows(path, fields):
    with Path(path).open(newline="", encoding="utf-8-sig") as stream:
        reader = csv.DictReader(stream)
        if reader.fieldnames is None or set(fields) - set(reader.fieldnames):
            raise InputError(f"{path}: required columns: {', '.join(fields)}")
        rows = list(reader)
    return rows


def decimal(value, label, *, minimum=None):
    try:
        result = Decimal(value)
    except (InvalidOperation, TypeError):
        raise InputError(f"{label}: invalid decimal {value!r}") from None
    if not result.is_finite() or (minimum is not None and result < minimum):
        raise InputError(f"{label}: out of range {value!r}")
    return result


def required(row, key, label):
    value = (row.get(key) or "").strip()
    if not value:
        raise InputError(f"{label}: {key} is required")
    return value


def round_up(value):
    return value.quantize(CENT, rounding=ROUND_CEILING)


def calculate(month, costs, allocations, parameters):
    if not re.fullmatch(r"\d{4}-(0[1-9]|1[0-2])", month):
        raise InputError("month must be YYYY-MM")
    totals = defaultdict(Decimal)
    coverage = set()
    shared = {}
    seen = set()
    provisional = False
    for row in costs:
        if row["month"] != month:
            continue
        item = required(row, "cost_id", "cost row")
        if item in seen:
            raise InputError(f"duplicate cost_id: {item}")
        seen.add(item)
        product = required(row, "product", item)
        category = required(row, "category", item)
        if product not in (*PRODUCTS, "shared") or category not in CATEGORIES:
            raise InputError(f"{item}: invalid product/category")
        basis = required(row, "basis", item)
        if basis not in ("actual", "estimate", "confirmed_zero"):
            raise InputError(f"{item}: basis must be actual, estimate or confirmed_zero")
        required(row, "source_ref", item)
        currency = required(row, "currency", item).upper()
        amount = decimal(row["amount"], f"{item} amount")
        fx = decimal(row["fx_to_cny"], f"{item} fx_to_cny", minimum=Decimal("0.0000000001"))
        if currency == "CNY" and fx != 1:
            raise InputError(f"{item}: CNY fx_to_cny must be one")
        if currency != "CNY":
            required(row, "fx_ref", item)
        if basis == "confirmed_zero" and amount != 0:
            raise InputError(f"{item}: confirmed_zero amount must be zero")
        provisional |= basis == "estimate"
        converted = amount * fx
        if product == "shared":
            shared[item] = (category, converted)
        else:
            totals[(product, category)] += converted
            coverage.add((product, category))

    weights = defaultdict(Decimal)
    allocated = defaultdict(Decimal)
    seen_allocations = set()
    for row in allocations:
        if row["month"] != month:
            continue
        item = required(row, "cost_id", "allocation row")
        product = required(row, "product", item)
        if item not in shared or product not in PRODUCTS:
            raise InputError(f"{item}: allocation must target a shared cost and a product")
        if (item, product) in seen_allocations:
            raise InputError(f"{item}: duplicate allocation to {product}")
        seen_allocations.add((item, product))
        required(row, "driver", item)
        required(row, "source_ref", item)
        weight = decimal(row["weight"], f"{item} weight", minimum=Decimal(0))
        if weight > 1:
            raise InputError(f"{item}: weight exceeds one")
        weights[item] += weight
        category, amount = shared[item]
        allocated[(product, category)] += amount * weight
        coverage.add((product, category))
    for item in shared:
        if weights[item] != 1:
            raise InputError(f"{item}: allocation weights sum to {weights[item]}, expected 1")

    missing = {(product, category) for product in PRODUCTS for category in CATEGORIES} - coverage
    if missing:
        raise InputError(f"missing category evidence: {sorted(missing)}")

    configs = {}
    for row in parameters:
        if row["month"] != month:
            continue
        product = required(row, "product", "parameter row")
        if product not in PRODUCTS or product in configs:
            raise InputError(f"invalid or duplicate product parameter: {product}")
        required(row, "unit_name", product)
        required(row, "source_ref", product)
        status = required(row, "approval_status", product)
        if status not in ("approved", "working"):
            raise InputError(f"{product}: approval_status must be approved or working")
        provisional |= status != "approved"
        units = decimal(row["billable_units"], f"{product} billable_units", minimum=Decimal("0.0000000001"))
        onboarding = decimal(row["onboarding_amort_cny"], f"{product} onboarding_amort_cny", minimum=Decimal(0))
        reserve = decimal(row["risk_reserve_pct"], f"{product} risk_reserve_pct", minimum=Decimal(0))
        fee = decimal(row["payment_fee_pct"], f"{product} payment_fee_pct", minimum=Decimal(0))
        margin = decimal(row["min_margin_pct"], f"{product} min_margin_pct", minimum=Decimal(0))
        if fee + margin >= 1:
            raise InputError(f"{product}: fee + margin must be below one")
        configs[product] = (units, onboarding, reserve, fee, margin, row["unit_name"])
    if set(configs) != set(PRODUCTS):
        raise InputError("parameters required for both products")

    result = {"month": month, "currency": "CNY", "status": "provisional" if provisional else "actual_inputs", "products": {}}
    for product in PRODUCTS:
        by_category = {category: totals[(product, category)] + allocated[(product, category)] for category in CATEGORIES}
        base = sum(by_category.values(), Decimal(0))
        if base < 0:
            raise InputError(f"{product}: negative monthly cost; review credits")
        units, onboarding, reserve, fee, margin, unit_name = configs[product]
        adjusted = base * (1 + reserve) + onboarding
        floor = adjusted / (1 - fee - margin)
        result["products"][product] = {
            "categories_cny": {k: str(v) for k, v in by_category.items()},
            "monthly_cost_cny": str(base),
            "quote_floor_ex_tax_cny": str(round_up(floor)),
            "unit_name": unit_name,
            "billable_units": str(units),
            "unit_floor_ex_tax_cny": str(round_up(floor / units)),
        }
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--month", required=True)
    parser.add_argument("--costs", required=True)
    parser.add_argument("--allocations", required=True)
    parser.add_argument("--parameters", required=True)
    args = parser.parse_args()
    costs = read_rows(args.costs, ("month", "cost_id", "product", "category", "amount", "currency", "fx_to_cny", "fx_ref", "basis", "source_ref"))
    allocations = read_rows(args.allocations, ("month", "cost_id", "product", "weight", "driver", "source_ref"))
    parameters = read_rows(args.parameters, ("month", "product", "billable_units", "unit_name", "onboarding_amort_cny", "risk_reserve_pct", "payment_fee_pct", "min_margin_pct", "approval_status", "source_ref"))
    print(json.dumps(calculate(args.month, costs, allocations, parameters), ensure_ascii=False, indent=2))


if __name__ == "__main__":
    try:
        main()
    except (InputError, OSError) as exc:
        raise SystemExit(f"costing: {exc}") from None
