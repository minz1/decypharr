#!/usr/bin/env node

const fs = require('fs');
const path = require('path');

const buildDir = {
    css: './pkg/server/assets/build/css',
    js: './pkg/server/assets/build/js',
    fonts: './pkg/server/assets/build/fonts'
};

// Create directories
Object.values(buildDir).forEach(dir => {
    if (!fs.existsSync(dir)) {
        fs.mkdirSync(dir, {recursive: true});
    }
});

async function fetchOK(url) {
    const response = await fetch(url);
    if (!response.ok) {
        throw new Error(`Failed to download ${url}: ${response.status}`);
    }
    return response;
}

async function downloadFile(url, filepath) {
    console.log(`📥 Downloading ${path.basename(filepath)}...`);
    const data = Buffer.from(await (await fetchOK(url)).arrayBuffer());
    fs.writeFileSync(filepath, data);
    console.log(`   ✓ Downloaded ${path.basename(filepath)} (${(data.length / 1024).toFixed(1)}KB)`);
}

async function downloadText(url) {
    return (await fetchOK(url)).text();
}

// Files to download
const downloads = [
    {
        url: 'https://cdn.jsdelivr.net/npm/bootstrap-icons@1.11.2/font/fonts/bootstrap-icons.woff',
        path: path.join(buildDir.fonts, 'bootstrap-icons.woff')
    },
    {
        url: 'https://cdn.jsdelivr.net/npm/bootstrap-icons@1.11.2/font/fonts/bootstrap-icons.woff2',
        path: path.join(buildDir.fonts, 'bootstrap-icons.woff2')
    },
    {
        url: 'https://code.jquery.com/jquery-3.7.1.min.js',
        path: path.join(buildDir.js, 'jquery-3.7.1.min.js')
    }
];

// Download all files
async function downloadAssets() {
    console.log('📦 Downloading external assets...\n');

    try {
        // Download Bootstrap Icons CSS and fix paths
        console.log('📥 Downloading Bootstrap Icons CSS...');
        const biCSS = await downloadText('https://cdn.jsdelivr.net/npm/bootstrap-icons@1.11.2/font/bootstrap-icons.css');

        // Fix font paths to point to our local fonts
        const fixedCSS = biCSS.replace(
            /url\("\.\/fonts\//g,
            'url("../fonts/'
        );

        // Write fixed CSS to source directory so it can be minified
        const biCSSSourcePath = path.join('./pkg/server/assets/css', 'bootstrap-icons.css');
        fs.writeFileSync(biCSSSourcePath, fixedCSS);
        console.log(`   ✓ Downloaded Bootstrap Icons CSS (${(fixedCSS.length / 1024).toFixed(1)}KB)`);

        // Download other assets
        for (const download of downloads) {
            await downloadFile(download.url, download.path);
        }

        console.log('\nExternal assets downloaded successfully!');

    } catch (error) {
        console.error('💥 Error downloading assets:', error);
        process.exit(1);
    }
}

downloadAssets();