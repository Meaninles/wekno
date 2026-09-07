import pytest

from app.budget import BudgetExhausted
from app.delivery import DeliveryError
from app.failures import error_code


@pytest.mark.parametrize("error,expected", [
    (BudgetExhausted("limit"), "task_limit"),
    (DeliveryError('Delivery requirements could not be satisfied: empty answer'), "empty_response"),
    (TimeoutError("private provider information"), "timeout"),
    (RuntimeError("HTTP 429 private credential must not escape"), "busy"),
    (RuntimeError("arbitrary private diagnostic"), "unknown"),
    (RuntimeError("workspace input id 8541320, line 429"), "unknown"),
    (ExceptionGroup("wrapped", [RuntimeError("connection refused")]), "connection"),
])
def test_private_exceptions_emit_only_known_public_code(error, expected):
    assert error_code(error) == expected
