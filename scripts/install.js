#!/usr/bin/env node

const fs = require('fs')
const os = require('os')
const path = require('path')
const https = require('https')
const { pipeline } = require('stream')
const { promisify } = require('util')
const { execFileSync } = require('child_process')

const streamPipeline = promisify(pipeline)

const pkg = require('../package.json')

const version = pkg.version
const binaryName = pkg.config.binaryName
const githubRepo = pkg.config.githubRepo

const mapPlatform = () => {
  const platform = process.platform
  const arch = process.arch
  if (platform === 'darwin' && arch === 'x64') return { goos: 'darwin', goarch: 'amd64', ext: 'tar.gz', bin: binaryName }
  if (platform === 'darwin' && arch === 'arm64') return { goos: 'darwin', goarch: 'arm64', ext: 'tar.gz', bin: binaryName }
  if (platform === 'linux' && arch === 'x64') return { goos: 'linux', goarch: 'amd64', ext: 'tar.gz', bin: binaryName }
  if (platform === 'linux' && arch === 'arm64') return { goos: 'linux', goarch: 'arm64', ext: 'tar.gz', bin: binaryName }
  if (platform === 'win32' && arch === 'x64') return { goos: 'windows', goarch: 'amd64', ext: 'zip', bin: `${binaryName}.exe` }
  if (platform === 'win32' && arch === 'arm64') return { goos: 'windows', goarch: 'arm64', ext: 'zip', bin: `${binaryName}.exe` }
  throw new Error(`Unsupported platform: ${platform}/${arch}`)
}

const download = (url, dest) => new Promise((resolve, reject) => {
  https.get(url, (res) => {
    if (res.statusCode >= 300 && res.statusCode < 400 && res.headers.location) {
      return download(res.headers.location, dest).then(resolve).catch(reject)
    }
    if (res.statusCode !== 200) {
      reject(new Error(`Download failed: ${res.statusCode} ${url}`))
      return
    }
    const file = fs.createWriteStream(dest)
    streamPipeline(res, file).then(resolve).catch(reject)
  }).on('error', reject)
})

async function main() {
  const target = mapPlatform()
  const asset = `${binaryName}_${version}_${target.goos}_${target.goarch}.${target.ext}`
  const url = `https://github.com/${githubRepo}/releases/download/v${version}/${asset}`

  const packageRoot = path.resolve(__dirname, '..')
  const runtimeDir = path.join(packageRoot, 'runtime')
  const tmpDir = path.join(packageRoot, '.tmp-install')
  const archivePath = path.join(tmpDir, asset)

  fs.mkdirSync(runtimeDir, { recursive: true })
  fs.mkdirSync(tmpDir, { recursive: true })

  console.log(`Downloading ${asset} from ${url}`)
  await download(url, archivePath)

  if (target.ext === 'zip') {
    const ps = `Expand-Archive -LiteralPath '${archivePath}' -DestinationPath '${runtimeDir}' -Force`
    execFileSync('powershell', ['-NoProfile', '-Command', ps], { stdio: 'inherit' })
  } else {
    execFileSync('tar', ['-xzf', archivePath, '-C', runtimeDir], { stdio: 'inherit' })
  }

  const installed = path.join(runtimeDir, target.bin)
  if (!fs.existsSync(installed)) {
    throw new Error(`Installed binary not found: ${installed}`)
  }

  if (process.platform !== 'win32') {
    fs.chmodSync(installed, 0o755)
  }

  fs.rmSync(tmpDir, { recursive: true, force: true })
  console.log(`Installed ${binaryName} to ${installed}`)
}

main().catch((err) => {
  console.error(err.message)
  process.exit(1)
})
