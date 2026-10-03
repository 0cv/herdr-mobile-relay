const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.22.4-408-af9cc67cdaa90220/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
