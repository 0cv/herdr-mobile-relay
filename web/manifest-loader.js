const setupToken = new URLSearchParams(location.hash.slice(1)).get('setup') || '';
const manifestLink = document.createElement('link');
manifestLink.rel = 'manifest';
manifestLink.href = navigator.standalone === false && setupToken.length >= 16 && setupToken.length <= 512
  ? 'setup.webmanifest'
  : 'manifest.webmanifest';
document.head.append(manifestLink);
