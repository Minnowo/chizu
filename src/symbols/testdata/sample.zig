const std = @import("std");

const Point = struct {
    x: i32,

    pub fn init() Point {
        return .{ .x = 0 };
    }
};

const Color = enum { red };

pub fn main() void {}
