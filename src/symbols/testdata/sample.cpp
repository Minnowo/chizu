namespace geo {

class Shape {
public:
    virtual double area() const { return 0; }
    ~Shape() {}
};

struct Point {
    int x;
};

double Circle::area() const {
    return 3.14;
}

int add(int a, int b) { return a + b; }

using Id = int;

}
