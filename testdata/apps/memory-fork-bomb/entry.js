function handle() {
  var generations = [];
  while (true) {
    generations.push("x".repeat(1024 * 1024) + String(generations.length));
  }
}
