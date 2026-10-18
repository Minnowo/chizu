<?php
namespace App;

interface Shape { public function area(); }

trait Greets { public function greet() {} }

class Greeter {
    public function hello() { return "hi"; }
}

function main() {}
