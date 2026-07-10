from __future__ import annotations

from app.oidc_mint import _abs_url, _extract_oauth_code_from_location


def test_extract_code_from_loopback():
    code, err, state = _extract_oauth_code_from_location(
        "http://127.0.0.1:19787/callback?code=abc123&state=xyz"
    )
    assert code == "abc123"
    assert err is None
    assert state == "xyz"


def test_extract_error():
    code, err, state = _extract_oauth_code_from_location(
        "http://127.0.0.1:19787/callback?error=login_required&error_description=need+login"
    )
    assert code is None
    assert err and "login_required" in err


def test_non_loopback():
    code, err, state = _extract_oauth_code_from_location(
        "https://auth.x.ai/login?x=1"
    )
    assert code is None and err is None


def test_abs_url():
    assert _abs_url("https://auth.x.ai/oauth2/authorize", "/foo").startswith(
        "https://auth.x.ai/"
    )
