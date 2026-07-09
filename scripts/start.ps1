# Start grok2api (Windows)
$ErrorActionPreference = "Stop"
Set-Location (Split-Path $PSScriptRoot -Parent)

if (-not (Test-Path .venv)) {
  Write-Host "[grok2api] creating .venv ..."
  python -m venv .venv
}
& .\.venv\Scripts\Activate.ps1
pip install -q -r requirements.txt

Write-Host "[grok2api] status:"
python -m app status

Write-Host "[grok2api] starting on http://127.0.0.1:8787"
python run.py
