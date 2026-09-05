[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$workspace = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$runtimeRoot = Join-Path $workspace 'build-assets\onnxruntime'
$releaseVersion = '1.29.0'
$packages = @(
	[pscustomobject]@{
		Architecture = 'amd64'
		ArchiveName = 'onnxruntime-linux-x64-1.29.0.tgz'
		ArchiveSHA256 = 'C3FDDC4F139A045B0C4902C57410F0694F1C2FDF9B6939FBE38B1AEAE7CD14BA'
		ArchiveRoot = 'onnxruntime-linux-x64-1.29.0'
		RuntimeSHA256 = '5715F06D8992CA8EEEDDCCE43DF3A7D38F97D537052126F558E912CB312460CA'
		ProvidersSHA256 = '086EC1D5388F64153D9C63470D126693DB9A182C8CE236D3A1119068471B8A0D'
	}
	[pscustomobject]@{
		Architecture = 'arm64'
		ArchiveName = 'onnxruntime-linux-aarch64-1.29.0.tgz'
		ArchiveSHA256 = 'E1799098EBC054B370F6176A450F158720F297818C613E5DC99B92E2EC82346F'
		ArchiveRoot = 'onnxruntime-linux-aarch64-1.29.0'
		RuntimeSHA256 = 'A27D21126DB312AA8F02F3D5EAEBE466E991F51F469882E6D0407D5A8B64AFDA'
		ProvidersSHA256 = '3B6BE288FBFB7DFF8770D08A23DEFDDE18E8F7E0F5A2B344A0E5E238C999EA88'
	}
)

function Assert-FileHash([string] $Path, [string] $ExpectedSHA256) {
	if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) {
		throw "ONNX Runtime dependency is missing: $Path"
	}
	$actual = (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash
	if ($actual -ne $ExpectedSHA256) {
		throw "ONNX Runtime dependency hash mismatch for $Path`: expected $ExpectedSHA256, got $actual"
	}
}

$temporaryRoot = Join-Path ([IO.Path]::GetTempPath()) ('qqtang-onnxruntime-' + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $temporaryRoot | Out-Null
try {
	foreach ($package in $packages) {
		$destination = Join-Path $runtimeRoot ("linux-{0}" -f $package.Architecture)
		$runtimeDestination = Join-Path $destination "libonnxruntime.so.$releaseVersion"
		$providersDestination = Join-Path $destination 'libonnxruntime_providers_shared.so'
		if ((Test-Path -LiteralPath $runtimeDestination -PathType Leaf) -and
			(Test-Path -LiteralPath $providersDestination -PathType Leaf)) {
			Assert-FileHash $runtimeDestination $package.RuntimeSHA256
			Assert-FileHash $providersDestination $package.ProvidersSHA256
			continue
		}

		$archivePath = Join-Path $temporaryRoot $package.ArchiveName
		$uri = "https://github.com/microsoft/onnxruntime/releases/download/v$releaseVersion/$($package.ArchiveName)"
		Invoke-WebRequest -Uri $uri -OutFile $archivePath -UseBasicParsing
		Assert-FileHash $archivePath $package.ArchiveSHA256

		$extractRoot = Join-Path $temporaryRoot $package.Architecture
		New-Item -ItemType Directory -Path $extractRoot | Out-Null
		& tar -xzf $archivePath -C $extractRoot `
			"$($package.ArchiveRoot)/lib/libonnxruntime.so.$releaseVersion" `
			"$($package.ArchiveRoot)/lib/libonnxruntime_providers_shared.so"
		if ($LASTEXITCODE -ne 0) {
			throw "Extracting ONNX Runtime for Linux/$($package.Architecture) exited with $LASTEXITCODE"
		}

		New-Item -ItemType Directory -Force -Path $destination | Out-Null
		Copy-Item -LiteralPath (Join-Path $extractRoot "$($package.ArchiveRoot)/lib/libonnxruntime.so.$releaseVersion") -Destination $runtimeDestination -Force
		Copy-Item -LiteralPath (Join-Path $extractRoot "$($package.ArchiveRoot)/lib/libonnxruntime_providers_shared.so") -Destination $providersDestination -Force
		Assert-FileHash $runtimeDestination $package.RuntimeSHA256
		Assert-FileHash $providersDestination $package.ProvidersSHA256
	}
}
finally {
	if (Test-Path -LiteralPath $temporaryRoot) {
		Remove-Item -LiteralPath $temporaryRoot -Recurse -Force
	}
}

Write-Host "ONNX Runtime $releaseVersion Linux amd64/arm64 dependencies are ready."
