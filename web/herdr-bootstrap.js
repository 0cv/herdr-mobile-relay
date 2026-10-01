const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.22.4-398-bc5f856f0c96e0c9/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
