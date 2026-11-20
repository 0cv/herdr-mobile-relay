const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.26.1-514-6f759d936c4170ca/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
