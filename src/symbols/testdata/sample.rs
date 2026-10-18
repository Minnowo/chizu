const LIMIT: u32 = 10;

struct Point {
    x: i32,
}

enum Color {
    Red,
}

trait Shape {
    fn area(&self) -> f64;
}

impl Point {
    fn new() -> Self {
        Point { x: 0 }
    }
}

impl Shape for Point {
    fn area(&self) -> f64 {
        0.0
    }
}

mod util {
    pub fn helper() {}
}

macro_rules! square {
    ($x:expr) => { $x * $x };
}

fn main() {}
